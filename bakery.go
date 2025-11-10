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
	"embed"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"bakery/pkg/database/memory"
	"bakery/pkg/version"
)

// uiFS embeds the single-page interface so deployments stay self-contained.
//
//go:embed ui/app.gohtml
var uiFS embed.FS

// ===== Entry Point and Configuration =====

// main wires configuration, persistence, services, and HTTP handlers into one process.
func main() {
	cfg := parseFlags()
	if cfg.showVersion {
		log.Printf("bakery version %s", version.Version())
		return
	}

	driverName, cleanup, err := memory.Register(cfg.dbType, cfg.dbPath)
	if err != nil {
		log.Fatalf("unable to register database driver: %v", err)
	}
	defer cleanup()

	db, err := sql.Open(driverName, cfg.dbPath)
	if err != nil {
		log.Fatalf("unable to open database: %v", err)
	}
	defer db.Close()

	if err := memory.EnsureSchema(context.Background(), db); err != nil {
		log.Fatalf("unable to ensure schema: %v", err)
	}

	orderRepo := &orderRepository{db: db}
	inventoryRepo := &inventoryRepository{db: db}

	orders := newOrderService(orderRepo)
	defer orders.Close()
	inventory := newInventoryService(inventoryRepo)
	defer inventory.Close()

	httpSrv, err := newHTTPServer(orders, inventory)
	if err != nil {
		log.Fatalf("unable to prepare HTTP server: %v", err)
	}

	if cfg.domain != "" {
		runDomainServers(cfg.domain, httpSrv)
		return
	}

	addr := cfg.address()
	server := &http.Server{
		Addr:         addr,
		Handler:      httpSrv.Handler(),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	log.Printf("Bakery service is running on %s", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server stopped unexpectedly: %v", err)
	}
}

// appConfig keeps CLI options grouped together.
type appConfig struct {
	showVersion bool
	domain      string
	port        int
	dbType      string
	dbPath      string
}

// address prefers $PORT to support platforms like Render.
func (c appConfig) address() string {
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return ":" + strconv.Itoa(c.port)
}

// parseFlags exposes the required configuration knobs without extra dependencies.
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

// runDomainServers spins up HTTPS and an HTTP redirect to support domain hosting.
func runDomainServers(domain string, srv *httpServer) {
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

// generateCertificate builds a temporary certificate so staging deployments remain encrypted.
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
			Organization: []string{"Bakery"},
		},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(24 * time.Hour),
		DNSNames:  []string{domain},
		KeyUsage:  x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, "", "", err
	}

	certOut, err := os.CreateTemp("", "cert.pem")
	if err != nil {
		return tls.Certificate{}, "", "", err
	}
	defer certOut.Close()

	keyOut, err := os.CreateTemp("", "key.pem")
	if err != nil {
		return tls.Certificate{}, "", "", err
	}
	defer keyOut.Close()

	if err := pemEncode(certOut, derBytes); err != nil {
		return tls.Certificate{}, "", "", err
	}
	if err := pemEncodeKey(keyOut, priv); err != nil {
		return tls.Certificate{}, "", "", err
	}

	cert, err := tls.LoadX509KeyPair(certOut.Name(), keyOut.Name())
	if err != nil {
		return tls.Certificate{}, "", "", err
	}
	return cert, keyOut.Name(), certOut.Name(), nil
}

// pemEncode wraps the certificate bytes in a PEM block because tls.LoadX509KeyPair expects files.
func pemEncode(file *os.File, derBytes []byte) error {
	if err := pem.Encode(file, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		return err
	}
	return nil
}

// pemEncodeKey serializes the private key in PEM format for the temporary TLS files.
func pemEncodeKey(file *os.File, key *ecdsa.PrivateKey) error {
	b, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return pem.Encode(file, &pem.Block{Type: "EC PRIVATE KEY", Bytes: b})
}

// ===== Domain Models =====

// OrderItem describes a requested product and the amount desired.
type OrderItem struct {
	Name     string `json:"name"`
	Quantity int    `json:"quantity"`
}

// BreadSchedule captures the recurring loaf delivery plan.
type BreadSchedule struct {
	Days      []string `json:"days"`
	Frequency string   `json:"frequency"`
	StartDate string   `json:"start_date"`
	Notes     string   `json:"notes"`
}

// CroissantSchedule tracks specific weekday drops for pastries.
type CroissantSchedule struct {
	Day      string `json:"day"`
	Quantity int    `json:"quantity"`
	Item     string `json:"item"`
}

// Order aggregates the complete delivery instructions for a household.
type Order struct {
	ID                int64               `json:"id"`
	CustomerName      string              `json:"customer_name"`
	Address           string              `json:"address"`
	Phone             string              `json:"phone"`
	Items             []OrderItem         `json:"items"`
	BreadSchedule     BreadSchedule       `json:"bread_schedule"`
	CroissantSchedule []CroissantSchedule `json:"croissant_schedule"`
	Comment           string              `json:"comment"`
	CreatedAt         time.Time           `json:"created_at"`
}

// MenuItem feeds the hero cards on the storefront.
type MenuItem struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Price       string `json:"price"`
	Image       string `json:"image"`
	Category    string `json:"category"`
}

// InventoryItem represents a baked batch visible to admins and customers.
type InventoryItem struct {
	ID             int64     `json:"id"`
	Name           string    `json:"name"`
	Category       string    `json:"category"`
	AvailableCount int       `json:"available_count"`
	PriceCents     int       `json:"price_cents"`
	BakedAt        time.Time `json:"baked_at"`
	CreatedAt      time.Time `json:"created_at"`
}

// ===== Persistence Layer =====

type orderRepository struct {
	db *sql.DB
}

// Save persists an order so the service can respond with the generated ID.
func (r *orderRepository) Save(ctx context.Context, order Order) (Order, error) {
	items, err := json.Marshal(order.Items)
	if err != nil {
		return Order{}, err
	}
	breadPlan, err := json.Marshal(order.BreadSchedule)
	if err != nil {
		return Order{}, err
	}
	croissantPlan, err := json.Marshal(order.CroissantSchedule)
	if err != nil {
		return Order{}, err
	}

	query := "INSERT INTO orders (name, address, phone, items, bread_schedule, croissant_schedule, comment) VALUES (?, ?, ?, ?, ?, ?, ?)"
	result, err := r.db.ExecContext(ctx, query, order.CustomerName, order.Address, order.Phone, string(items), string(breadPlan), string(croissantPlan), order.Comment)
	if err != nil {
		return Order{}, err
	}

	id, err := result.LastInsertId()
	if err != nil {
		return Order{}, err
	}

	order.ID = id
	order.CreatedAt = time.Now().UTC()
	return order, nil
}

// List returns every order in reverse ID order for the admin board.
func (r *orderRepository) List(ctx context.Context) ([]Order, error) {
	query := "SELECT id, name, address, phone, items, bread_schedule, croissant_schedule, comment FROM orders ORDER BY id DESC"
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var orders []Order
	for rows.Next() {
		var (
			order         Order
			itemsData     string
			breadData     string
			croissantData string
		)

		if err := rows.Scan(&order.ID, &order.CustomerName, &order.Address, &order.Phone, &itemsData, &breadData, &croissantData, &order.Comment); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(itemsData), &order.Items); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(breadData), &order.BreadSchedule); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(croissantData), &order.CroissantSchedule); err != nil {
			return nil, err
		}
		orders = append(orders, order)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return orders, nil
}

type inventoryRepository struct {
	db *sql.DB
}

// Save inserts a freshly baked batch for later listing.
func (r *inventoryRepository) Save(ctx context.Context, item InventoryItem) (InventoryItem, error) {
	query := "INSERT INTO inventory (name, category, available_count, price_cents, baked_at) VALUES (?, ?, ?, ?, ?)"
	result, err := r.db.ExecContext(ctx, query, item.Name, item.Category, item.AvailableCount, item.PriceCents, item.BakedAt.UTC())
	if err != nil {
		return InventoryItem{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return InventoryItem{}, err
	}
	item.ID = id
	item.CreatedAt = time.Now().UTC()
	return item, nil
}

// List fetches all batches so both the admin panel and the storefront stay in sync.
func (r *inventoryRepository) List(ctx context.Context) ([]InventoryItem, error) {
	query := "SELECT id, name, category, available_count, price_cents, baked_at FROM inventory ORDER BY baked_at DESC"
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []InventoryItem
	for rows.Next() {
		var item InventoryItem
		var bakedAt time.Time
		if err := rows.Scan(&item.ID, &item.Name, &item.Category, &item.AvailableCount, &item.PriceCents, &bakedAt); err != nil {
			return nil, err
		}
		item.BakedAt = bakedAt.UTC()
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// Update edits counts and pricing when the admin adjusts availability.
func (r *inventoryRepository) Update(ctx context.Context, item InventoryItem) error {
	query := "UPDATE inventory SET available_count = ?, price_cents = ?, baked_at = ? WHERE id = ?"
	result, err := r.db.ExecContext(ctx, query, item.AvailableCount, item.PriceCents, item.BakedAt.UTC(), item.ID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return errInventoryNotFound
	}
	return nil
}

// Delete removes a batch entirely once it is sold out.
func (r *inventoryRepository) Delete(ctx context.Context, id int64) error {
	result, err := r.db.ExecContext(ctx, "DELETE FROM inventory WHERE id = ?", id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return errInventoryNotFound
	}
	return nil
}

// errInventoryNotFound signals missing rows for update and delete operations.
var errInventoryNotFound = errors.New("inventory item not found")

// ===== Order Service =====

type orderCommand struct {
	order Order
	reply chan orderCommandResult
}

type orderQuery struct {
	reply chan orderQueryResult
}

type orderCommandResult struct {
	order Order
	err   error
}

type orderQueryResult struct {
	orders []Order
	err    error
}

// validationError separates business rule failures from infrastructure issues.
type validationError struct {
	message string
}

func (e validationError) Error() string { return e.message }

// newValidationError keeps message creation centralised.
func newValidationError(msg string) error {
	return validationError{message: msg}
}

// isValidationError helps HTTP handlers pick the right status code.
func isValidationError(err error) bool {
	var v validationError
	return errors.As(err, &v)
}

// orderService handles submissions via a background goroutine and channels.
type orderService struct {
	repo          *orderRepository
	commands      chan orderCommand
	queries       chan orderQuery
	cancellations chan struct{}
}

// newOrderService starts the goroutine immediately to avoid blocking callers.
func newOrderService(repo *orderRepository) *orderService {
	svc := &orderService{
		repo:          repo,
		commands:      make(chan orderCommand),
		queries:       make(chan orderQuery),
		cancellations: make(chan struct{}),
	}
	go svc.loop()
	return svc
}

// loop processes order commands while honouring the Go proverb about communicating over channels.
func (s *orderService) loop() {
	for {
		select {
		case cmd := <-s.commands:
			if err := validateOrder(cmd.order); err != nil {
				cmd.reply <- orderCommandResult{err: err}
				continue
			}
			stored, err := s.repo.Save(context.Background(), cmd.order)
			cmd.reply <- orderCommandResult{order: stored, err: err}
		case q := <-s.queries:
			orders, err := s.repo.List(context.Background())
			q.reply <- orderQueryResult{orders: orders, err: err}
		case <-s.cancellations:
			return
		}
	}
}

// Submit registers an order asynchronously and waits for completion with a timeout.
func (s *orderService) Submit(ctx context.Context, order Order) (Order, error) {
	reply := make(chan orderCommandResult)
	cmd := orderCommand{order: order, reply: reply}

	select {
	case s.commands <- cmd:
	case <-ctx.Done():
		return Order{}, ctx.Err()
	case <-time.After(2 * time.Second):
		return Order{}, errors.New("queue is busy processing other orders")
	}

	select {
	case res := <-reply:
		return res.order, res.err
	case <-ctx.Done():
		return Order{}, ctx.Err()
	case <-time.After(2 * time.Second):
		return Order{}, errors.New("order processing took too long")
	}
}

// List returns all stored orders using the background goroutine.
func (s *orderService) List(ctx context.Context) ([]Order, error) {
	reply := make(chan orderQueryResult)
	req := orderQuery{reply: reply}

	select {
	case s.queries <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(2 * time.Second):
		return nil, errors.New("queue is busy processing other orders")
	}

	select {
	case res := <-reply:
		return res.orders, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(2 * time.Second):
		return nil, errors.New("listing orders took too long")
	}
}

// Close stops the goroutine when the program exits.
func (s *orderService) Close() {
	close(s.cancellations)
}

// validateOrder keeps business rules close to the order service.
func validateOrder(order Order) error {
	if strings.TrimSpace(order.CustomerName) == "" {
		return newValidationError("name is required")
	}
	if strings.TrimSpace(order.Address) == "" {
		return newValidationError("address is required")
	}
	if strings.TrimSpace(order.Phone) == "" {
		return newValidationError("phone is required")
	}
	if len(order.Items) == 0 {
		return newValidationError("at least one item is required")
	}
	if len(order.BreadSchedule.Days) == 0 {
		return newValidationError("select at least one bread delivery day")
	}
	if order.BreadSchedule.Frequency == "" {
		return newValidationError("select a bread delivery frequency")
	}
	if strings.TrimSpace(order.BreadSchedule.StartDate) == "" {
		return newValidationError("select a bread start date")
	}
	if len(order.CroissantSchedule) == 0 {
		return newValidationError("select croissant days")
	}
	for _, slot := range order.CroissantSchedule {
		if slot.Day == "" {
			return newValidationError("croissant day is required")
		}
		if slot.Quantity <= 0 {
			return newValidationError("croissant quantity must be positive")
		}
	}
	return nil
}

// ===== Inventory Service =====

type inventoryCommand struct {
	action string
	item   InventoryItem
	id     int64
	reply  chan inventoryCommandResult
}

type inventoryQuery struct {
	reply chan inventoryQueryResult
}

type inventoryCommandResult struct {
	item InventoryItem
	err  error
}

type inventoryQueryResult struct {
	items []InventoryItem
	err   error
}

// inventoryService manages stock changes via channels to avoid locks.
type inventoryService struct {
	repo      *inventoryRepository
	commands  chan inventoryCommand
	listCalls chan inventoryQuery
	quit      chan struct{}
}

// newInventoryService launches the goroutine for background work.
func newInventoryService(repo *inventoryRepository) *inventoryService {
	svc := &inventoryService{
		repo:      repo,
		commands:  make(chan inventoryCommand),
		listCalls: make(chan inventoryQuery),
		quit:      make(chan struct{}),
	}
	go svc.loop()
	return svc
}

// loop serialises commands and queries so state stays consistent.
func (s *inventoryService) loop() {
	for {
		select {
		case cmd := <-s.commands:
			switch cmd.action {
			case "save":
				stored, err := s.repo.Save(context.Background(), cmd.item)
				cmd.reply <- inventoryCommandResult{item: stored, err: err}
			case "update":
				err := s.repo.Update(context.Background(), cmd.item)
				cmd.reply <- inventoryCommandResult{err: err}
			case "delete":
				err := s.repo.Delete(context.Background(), cmd.id)
				cmd.reply <- inventoryCommandResult{err: err}
			default:
				cmd.reply <- inventoryCommandResult{err: errors.New("unknown inventory action")}
			}
		case q := <-s.listCalls:
			items, err := s.repo.List(context.Background())
			q.reply <- inventoryQueryResult{items: items, err: err}
		case <-s.quit:
			return
		}
	}
}

// Add records a new batch and returns the stored item.
func (s *inventoryService) Add(ctx context.Context, item InventoryItem) (InventoryItem, error) {
	reply := make(chan inventoryCommandResult)
	cmd := inventoryCommand{action: "save", item: item, reply: reply}

	select {
	case s.commands <- cmd:
	case <-ctx.Done():
		return InventoryItem{}, ctx.Err()
	case <-time.After(2 * time.Second):
		return InventoryItem{}, errors.New("inventory queue is busy")
	}

	select {
	case res := <-reply:
		return res.item, res.err
	case <-ctx.Done():
		return InventoryItem{}, ctx.Err()
	case <-time.After(2 * time.Second):
		return InventoryItem{}, errors.New("inventory save timed out")
	}
}

// Update adjusts counts or pricing for an existing batch.
func (s *inventoryService) Update(ctx context.Context, item InventoryItem) error {
	reply := make(chan inventoryCommandResult)
	cmd := inventoryCommand{action: "update", item: item, reply: reply}

	select {
	case s.commands <- cmd:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
		return errors.New("inventory queue is busy")
	}

	select {
	case res := <-reply:
		return res.err
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
		return errors.New("inventory update timed out")
	}
}

// Delete removes a batch completely.
func (s *inventoryService) Delete(ctx context.Context, id int64) error {
	reply := make(chan inventoryCommandResult)
	cmd := inventoryCommand{action: "delete", id: id, reply: reply}

	select {
	case s.commands <- cmd:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
		return errors.New("inventory queue is busy")
	}

	select {
	case res := <-reply:
		return res.err
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
		return errors.New("inventory delete timed out")
	}
}

// List returns all batches via the background goroutine.
func (s *inventoryService) List(ctx context.Context) ([]InventoryItem, error) {
	reply := make(chan inventoryQueryResult)
	q := inventoryQuery{reply: reply}

	select {
	case s.listCalls <- q:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(2 * time.Second):
		return nil, errors.New("inventory queue is busy")
	}

	select {
	case res := <-reply:
		return res.items, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(2 * time.Second):
		return nil, errors.New("inventory list timed out")
	}
}

// Close stops the background worker when shutting down.
func (s *inventoryService) Close() {
	close(s.quit)
}

// ===== HTTP Layer =====

type httpServer struct {
	orders    *orderService
	inventory *inventoryService
	page      *template.Template
	heroMenu  []MenuItem
}

// newHTTPServer parses templates once so handlers stay fast.
func newHTTPServer(orders *orderService, inventory *inventoryService) (*httpServer, error) {
	tmpl, err := template.ParseFS(uiFS, "ui/app.gohtml")
	if err != nil {
		return nil, err
	}
	return &httpServer{
		orders:    orders,
		inventory: inventory,
		page:      tmpl,
		heroMenu:  defaultMenu(),
	}, nil
}

// Handler builds the mux for both public and admin endpoints.
func (s *httpServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", s.pageHandler("customer"))
	mux.Handle("/admin", s.pageHandler("admin"))
	mux.Handle("/api/orders", s.ordersEndpoint())
	mux.Handle("/api/menu", s.menuEndpoint())
	mux.Handle("/api/admin/inventory", s.inventoryEndpoint())
	return mux
}

// pageHandler renders the single-page HTML with embedded JSON bootstrapping.
func (s *httpServer) pageHandler(page string) http.Handler {
	type viewData struct {
		Page           string
		FreeDelivery   string
		CroissantBlurb string
		MenuJSON       template.JS
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		menu := s.resolveMenu(r.Context())
		payload, err := json.Marshal(menu)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		data := viewData{
			Page:           page,
			FreeDelivery:   "Бесплатная доставка по району Белая Ромашка каждое утро",
			CroissantBlurb: "Запланируйте хлеб и круассаны, мы привезем к утреннему чаю",
			MenuJSON:       template.JS(string(payload)),
		}
		if err := s.page.Execute(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	})
}

// ordersEndpoint handles POST for new orders and GET for admin listings.
func (s *httpServer) ordersEndpoint() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			s.createOrder(w, r)
		case http.MethodGet:
			s.listOrders(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

// menuEndpoint returns the current menu as JSON.
func (s *httpServer) menuEndpoint() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		items, err := s.inventory.List(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		menu := make([]MenuItem, 0, len(items))
		for _, item := range items {
			menu = append(menu, MenuItem{
				Name:        item.Name,
				Description: fmt.Sprintf("Свежая партия от %s", item.BakedAt.Format("02.01 15:04")),
				Price:       formatPrice(item.PriceCents),
				Image:       imageForCategory(item.Category),
				Category:    item.Category,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(menu)
	})
}

// inventoryEndpoint covers admin CRUD endpoints.
func (s *httpServer) inventoryEndpoint() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			s.createInventory(w, r)
		case http.MethodPut:
			s.updateInventory(w, r)
		case http.MethodDelete:
			s.deleteInventory(w, r)
		case http.MethodGet:
			s.listInventory(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

// createOrder validates input and forwards it to the order service.
func (s *httpServer) createOrder(w http.ResponseWriter, r *http.Request) {
	type schedulePayload struct {
		Frequency string   `json:"frequency"`
		Days      []string `json:"days"`
		StartDate string   `json:"startDate"`
		Notes     string   `json:"notes"`
	}
	type croissantPayload struct {
		Day      string `json:"day"`
		Quantity int    `json:"quantity"`
		Item     string `json:"item"`
	}
	type itemPayload struct {
		Name     string `json:"name"`
		Quantity int    `json:"quantity"`
	}
	type orderPayload struct {
		Name              string             `json:"name"`
		Phone             string             `json:"phone"`
		Address           string             `json:"address"`
		BreadSchedule     schedulePayload    `json:"breadSchedule"`
		CroissantSchedule []croissantPayload `json:"croissantSchedule"`
		Items             []itemPayload      `json:"items"`
		Comment           string             `json:"comment"`
	}

	var payload orderPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	schedule := make([]CroissantSchedule, 0, len(payload.CroissantSchedule))
	for _, slot := range payload.CroissantSchedule {
		if strings.TrimSpace(slot.Day) == "" {
			http.Error(w, "day is required", http.StatusBadRequest)
			return
		}
		if slot.Quantity <= 0 {
			http.Error(w, "quantity must be a positive number", http.StatusBadRequest)
			return
		}
		schedule = append(schedule, CroissantSchedule{
			Day:      slot.Day,
			Quantity: slot.Quantity,
			Item:     slot.Item,
		})
	}

	items := make([]OrderItem, 0, len(payload.Items))
	for _, item := range payload.Items {
		if strings.TrimSpace(item.Name) == "" {
			http.Error(w, "item name is required", http.StatusBadRequest)
			return
		}
		if item.Quantity <= 0 {
			http.Error(w, "item quantity must be positive", http.StatusBadRequest)
			return
		}
		items = append(items, OrderItem{
			Name:     item.Name,
			Quantity: item.Quantity,
		})
	}

	request := Order{
		CustomerName: payload.Name,
		Phone:        payload.Phone,
		Address:      payload.Address,
		Items:        items,
		BreadSchedule: BreadSchedule{
			Frequency: payload.BreadSchedule.Frequency,
			Days:      payload.BreadSchedule.Days,
			StartDate: payload.BreadSchedule.StartDate,
			Notes:     payload.BreadSchedule.Notes,
		},
		CroissantSchedule: schedule,
		Comment:           payload.Comment,
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	stored, err := s.orders.Submit(ctx, request)
	if err != nil {
		if isValidationError(err) {
			s.respondError(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.respondError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("Stored order %d for %s", stored.ID, stored.CustomerName)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stored)
}

// listOrders returns all orders for admin review.
func (s *httpServer) listOrders(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	orders, err := s.orders.List(ctx)
	if err != nil {
		s.respondError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(orders)
}

// createInventory adds a new batch to the catalog.
func (s *httpServer) createInventory(w http.ResponseWriter, r *http.Request) {
	var payload inventoryPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		s.respondError(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if err := payload.Validate(); err != nil {
		s.respondError(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	item := InventoryItem{
		Name:           payload.Name,
		Category:       payload.Category,
		BakedAt:        payload.BakedAt,
		PriceCents:     payload.PriceCents,
		AvailableCount: payload.Quantity,
	}
	stored, err := s.inventory.Add(ctx, item)
	if err != nil {
		s.respondError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("Added inventory %d %s", stored.ID, stored.Name)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stored)
}

// updateInventory edits an existing batch.
func (s *httpServer) updateInventory(w http.ResponseWriter, r *http.Request) {
	var payload inventoryPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		s.respondError(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if payload.ID == 0 {
		s.respondError(w, "id is required", http.StatusBadRequest)
		return
	}
	if err := payload.Validate(); err != nil {
		s.respondError(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	item := InventoryItem{
		ID:             int64(payload.ID),
		Name:           payload.Name,
		Category:       payload.Category,
		BakedAt:        payload.BakedAt,
		PriceCents:     payload.PriceCents,
		AvailableCount: payload.Quantity,
	}
	if err := s.inventory.Update(ctx, item); err != nil {
		if errors.Is(err, errInventoryNotFound) {
			s.respondError(w, err.Error(), http.StatusNotFound)
			return
		}
		s.respondError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("Updated inventory %d", payload.ID)
	w.WriteHeader(http.StatusNoContent)
}

// deleteInventory removes a batch entirely.
func (s *httpServer) deleteInventory(w http.ResponseWriter, r *http.Request) {
	rawID := r.URL.Query().Get("id")
	if rawID == "" {
		s.respondError(w, "id is required", http.StatusBadRequest)
		return
	}
	id, err := strconv.Atoi(rawID)
	if err != nil {
		s.respondError(w, "invalid id", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	if err := s.inventory.Delete(ctx, int64(id)); err != nil {
		if errors.Is(err, errInventoryNotFound) {
			s.respondError(w, err.Error(), http.StatusNotFound)
			return
		}
		s.respondError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("Deleted inventory %d", id)
	w.WriteHeader(http.StatusNoContent)
}

// listInventory lists batches for the admin console.
func (s *httpServer) listInventory(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	items, err := s.inventory.List(ctx)
	if err != nil {
		s.respondError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	response := make([]inventoryResponse, 0, len(items))
	for _, item := range items {
		response = append(response, inventoryResponse{
			ID:       int(item.ID),
			Name:     item.Name,
			Category: item.Category,
			BakedAt:  item.BakedAt.Format("2006-01-02 15:04"),
			Price:    formatPrice(item.PriceCents),
			Quantity: item.AvailableCount,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// respondError keeps error responses consistent across endpoints.
func (s *httpServer) respondError(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// resolveMenu loads menu items from inventory or falls back to the hero list.
func (s *httpServer) resolveMenu(ctx context.Context) []MenuItem {
	items, err := s.inventory.List(ctx)
	if err != nil || len(items) == 0 {
		return s.heroMenu
	}
	menu := make([]MenuItem, 0, len(items))
	for _, item := range items {
		menu = append(menu, MenuItem{
			Name:        item.Name,
			Description: fmt.Sprintf("Свежая партия от %s", item.BakedAt.Format("02.01 15:04")),
			Price:       formatPrice(item.PriceCents),
			Image:       imageForCategory(item.Category),
			Category:    item.Category,
		})
	}
	return menu
}

// inventoryPayload handles JSON parsing for admin mutations.
type inventoryPayload struct {
	ID          int       `json:"id"`
	Name        string    `json:"name"`
	Category    string    `json:"category"`
	BakedAtRaw  string    `json:"baked_at"`
	PriceRaw    string    `json:"price_rub"`
	QuantityRaw string    `json:"quantity"`
	BakedAt     time.Time `json:"-"`
	PriceCents  int       `json:"-"`
	Quantity    int       `json:"-"`
}

// Validate parses incoming strings and ensures requirements are met.
func (p *inventoryPayload) Validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("name is required")
	}
	if strings.TrimSpace(p.Category) == "" {
		return errors.New("category is required")
	}
	if strings.TrimSpace(p.BakedAtRaw) == "" {
		return errors.New("baked_at is required")
	}
	baked, err := time.Parse("2006-01-02 15:04", p.BakedAtRaw)
	if err != nil {
		return fmt.Errorf("invalid baked_at: %w", err)
	}
	if strings.TrimSpace(p.PriceRaw) == "" {
		return errors.New("price_rub is required")
	}
	priceFloat, err := strconv.ParseFloat(strings.ReplaceAll(p.PriceRaw, ",", "."), 64)
	if err != nil {
		return fmt.Errorf("invalid price_rub: %w", err)
	}
	qty, err := strconv.Atoi(strings.TrimSpace(p.QuantityRaw))
	if err != nil || qty <= 0 {
		return errors.New("quantity must be positive")
	}
	p.BakedAt = baked
	p.PriceCents = int(priceFloat * 100)
	p.Quantity = qty
	return nil
}

// inventoryResponse exposes inventory info for the admin table.
type inventoryResponse struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
	BakedAt  string `json:"baked_at"`
	Price    string `json:"price"`
	Quantity int    `json:"quantity"`
}

// defaultMenu provides a friendly starting point when no inventory is loaded.
func defaultMenu() []MenuItem {
	return []MenuItem{
		{Name: "Сливочный круассан", Description: "Слойки с фермерским маслом", Price: "220 ₽", Image: "classic", Category: "croissant"},
		{Name: "Хрустящий багет", Description: "Пары хватает на утренний стол", Price: "160 ₽", Image: "baguette", Category: "bread"},
		{Name: "Шоколадный десерт", Description: "Горький шоколад 70%", Price: "250 ₽", Image: "chocolate", Category: "pastry"},
	}
}

// imageForCategory picks icons so the UI remains expressive.
func imageForCategory(category string) string {
	switch category {
	case "bread":
		return "baguette"
	case "croissant":
		return "classic"
	case "pastry":
		return "chocolate"
	default:
		return "hero"
	}
}

// formatPrice renders rubles with the ₽ sign.
func formatPrice(cents int) string {
	roubles := cents / 100
	remainder := cents % 100
	return fmt.Sprintf("%d,%02d ₽", roubles, remainder)
}
