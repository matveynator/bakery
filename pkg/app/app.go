package app

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
	"errors"
	"flag"
	"fmt"
	"io"
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

// Config captures CLI flags so the bakery service can run with a single Run call.
type Config struct {
	showVersion bool
	domain      string
	port        int
	dbType      string
	dbPath      string
}

// Run composes persistence, domain services, and the HTTP server using only standard library pieces.
func Run(ctx context.Context, args []string, logger *log.Logger) error {
	if logger == nil {
		// The logger is optional so tests can remain quiet while production still reports activity.
		logger = log.New(os.Stdout, "[bakery] ", log.LstdFlags)
	}

	cfg, err := parseFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// Help output is already printed by the flag package, so we quietly exit.
			return nil
		}
		return err
	}

	if cfg.showVersion {
		logger.Printf("bakery version %s", version.Version())
		return nil
	}

	driverName, cleanupDriver, err := memorydriver.Register(cfg.dbType, cfg.dbPath)
	if err != nil {
		return fmt.Errorf("unable to register database driver: %w", err)
	}
	defer cleanupDriver()

	db, err := sql.Open(driverName, cfg.dbPath)
	if err != nil {
		return fmt.Errorf("unable to open database: %w", err)
	}
	defer db.Close()

	if err := memorydriver.EnsureSchema(ctx, db); err != nil {
		return fmt.Errorf("unable to ensure schema: %w", err)
	}

	orderRepo := order.NewRepository(db)
	inventoryRepo := inventory.NewRepository(db)

	orderService := order.NewService(orderRepo)
	defer orderService.Close()

	inventoryService := inventory.NewService(inventoryRepo)
	defer inventoryService.Close()

	srv, err := httpapi.New(orderService, inventoryService, logger)
	if err != nil {
		return fmt.Errorf("unable to build http server: %w", err)
	}

	if cfg.domain != "" {
		logger.Printf("starting HTTPS servers for domain %s", cfg.domain)
		return runDomainServers(ctx, cfg.domain, srv, logger)
	}

	addr := cfg.address()
	server := &http.Server{
		Addr:         addr,
		Handler:      srv.Handler(),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	logger.Printf("Bakery service is running on %s", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server stopped unexpectedly: %w", err)
	}
	return nil
}

// address converts CLI port configuration into a binding string.
func (c Config) address() string {
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return ":" + strconv.Itoa(c.port)
}

// parseFlags uses a dedicated FlagSet so Run can be called from multiple entry points.
func parseFlags(args []string) (Config, error) {
	set := flag.NewFlagSet("bakery", flag.ContinueOnError)
	set.SetOutput(io.Discard)

	var cfg Config
	set.BoolVar(&cfg.showVersion, "version", false, "Show the application version")
	set.StringVar(&cfg.domain, "domain", "", "Serve HTTPS on 80/443 via Let's Encrypt when a domain is provided.")
	set.IntVar(&cfg.port, "port", 8765, "Port for running the HTTP server when not using -domain.")
	set.StringVar(&cfg.dbType, "db-type", "sqlite", "Database driver: chai, sqlite, duckdb, pgx (PostgreSQL), or clickhouse")
	set.StringVar(&cfg.dbPath, "db-path", "", "Filesystem path for chai/sqlite/duckdb databases; defaults to the working directory.")

	if err := set.Parse(args); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// runDomainServers launches both HTTP redirect and HTTPS handlers when a domain is configured.
func runDomainServers(ctx context.Context, domain string, srv *httpapi.Server, logger *log.Logger) error {
	tlsCert, keyFile, certFile, err := generateCertificate(domain)
	if err != nil {
		return fmt.Errorf("unable to generate certificate: %w", err)
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
		// The redirect server keeps HTTP clients pointed at HTTPS, so we log its lifecycle too.
		logger.Printf("HTTP redirect server listening on :80")
		if err := httpRedirect.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Printf("redirect server stopped: %v", err)
		}
	}()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpRedirect.Shutdown(shutdownCtx)
		httpsServer.Shutdown(shutdownCtx)
	}()

	logger.Printf("HTTPS server for %s is starting with an ephemeral certificate", domain)
	if err := httpsServer.ListenAndServeTLS(certFile, keyFile); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("TLS server stopped unexpectedly: %w", err)
	}
	return nil
}

// generateCertificate produces a temporary certificate so TLS works even before Let's Encrypt provisions.
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

// writeTempFile persists certificate data because the HTTP server expects file paths.
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
