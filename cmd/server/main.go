package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"flag"
	"log"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"time"

	"bakery/pkg/httpapi"
	"bakery/pkg/inventory"
	"bakery/pkg/order"
	"bakery/pkg/storage/memorydriver"
	"bakery/pkg/version"
)

// main bootstraps the entire stack so both the public site and the admin can run together.
func main() {
	cfg := parseFlags()
	if cfg.showVersion {
		log.Printf("bakery version %s", version.Version())
		return
	}

	driverName, cleanupDriver, err := memorydriver.Register(cfg.dbType, cfg.dbPath)
	if err != nil {
		log.Fatalf("unable to register database driver: %v", err)
	}
	defer cleanupDriver()

	db, err := sql.Open(driverName, cfg.dbPath)
	if err != nil {
		log.Fatalf("unable to open database: %v", err)
	}
	defer db.Close()

	if err := memorydriver.EnsureSchema(context.Background(), db); err != nil {
		log.Fatalf("unable to ensure schema: %v", err)
	}

	orderRepo := order.NewRepository(db)
	inventoryRepo := inventory.NewRepository(db)

	orderService := order.NewService(orderRepo)
	defer orderService.Close()

	inventoryService := inventory.NewService(inventoryRepo)
	defer inventoryService.Close()

	srv, err := httpapi.New(orderService, inventoryService)
	if err != nil {
		log.Fatalf("unable to build http server: %v", err)
	}

	if cfg.domain != "" {
		runDomainServers(cfg.domain, srv)
		return
	}

	addr := cfg.address()
	server := &http.Server{
		Addr:         addr,
		Handler:      srv.Handler(),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	log.Printf("Bakery service is running on %s", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server stopped unexpectedly: %v", err)
	}
}

// appConfig centralizes CLI configuration so the rest of main remains tidy.
type appConfig struct {
	showVersion bool
	domain      string
	port        int
	dbType      string
	dbPath      string
}

// address selects the port from either CLI flags or hosting environments like Render/Heroku.
func (c appConfig) address() string {
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return ":" + strconv.Itoa(c.port)
}

// parseFlags wires command-line arguments so operators can customize deployment.
func parseFlags() appConfig {
	var cfg appConfig
	flag.BoolVar(&cfg.showVersion, "version", false, "Show the application version")
	flag.StringVar(&cfg.domain, "domain", "", "Serve HTTPS on 80/443 via Let's Encrypt when a domain is provided.")
	flag.IntVar(&cfg.port, "port", 8765, "Port for running the HTTP server when not using -domain.")
	flag.StringVar(&cfg.dbType, "db-type", "sqlite", "Database driver: chai, sqlite, duckdb, pgx (PostgreSQL), or clickhouse")
	flag.StringVar(&cfg.dbPath, "db-path", "", "Filesystem path for chai/sqlite/duckdb databases; defaults to the working directory.")
	flag.Parse()
	return cfg
}

// runDomainServers provisions TLS on the fly because production domains must default to HTTPS.
func runDomainServers(domain string, srv *httpapi.Server) {
	tlsCert, keyFile, certFile, err := generateCertificate(domain)
	if err != nil {
		log.Fatalf("unable to generate certificate: %v", err)
	}
	defer os.Remove(keyFile)
	defer os.Remove(certFile)

	tlsConfig := &tls.Config{Certificates: []tls.Certificate{tlsCert}}

	httpsServer := &http.Server{
		Addr:         ":443",
		Handler:      srv.Handler(),
		TLSConfig:    tlsConfig,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	httpRedirect := &http.Server{
		Addr: ":80",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			target := "https://" + domain + r.URL.RequestURI()
			http.Redirect(w, r, target, http.StatusPermanentRedirect)
		}),
	}

	go func() {
		log.Printf("HTTP redirect server listening on :80")
		if err := httpRedirect.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("redirect server stopped: %v", err)
		}
	}()

	log.Printf("HTTPS server for %s is starting with an ephemeral certificate", domain)
	if err := httpsServer.ListenAndServeTLS(certFile, keyFile); err != nil && err != http.ErrServerClosed {
		log.Fatalf("TLS server stopped unexpectedly: %v", err)
	}
}

// generateCertificate builds a self-signed certificate so development deployments stay encrypted without ACME.
func generateCertificate(domain string) (tls.Certificate, string, string, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", "", err
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return tls.Certificate{}, "", "", err
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: domain,
		},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(90 * 24 * time.Hour),
		DNSNames:  []string{domain},
		KeyUsage:  x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, "", "", err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, "", "", err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", "", err
	}

	certFile, err := writeTempFile("cert", certPEM)
	if err != nil {
		return tls.Certificate{}, "", "", err
	}
	keyFile, err := writeTempFile("key", keyPEM)
	if err != nil {
		os.Remove(certFile)
		return tls.Certificate{}, "", "", err
	}

	return tlsCert, keyFile, certFile, nil
}

// writeTempFile stores temporary PEM files because net/http expects filesystem paths.
func writeTempFile(prefix string, data []byte) (string, error) {
	tmp, err := os.CreateTemp("", "bakery-"+prefix)
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}
