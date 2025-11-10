package httpapi

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"bakery/pkg/inventory"
	"bakery/pkg/order"
)

// uiFS packs the single page experience so deployments ship one binary.
// The assets live under public_html to satisfy the deployment expectation while keeping everything embedded.
//
//go:embed public_html/app.gohtml
var uiFS embed.FS

// Server wires HTTP endpoints to the asynchronous order and inventory services.
type Server struct {
	orders    *order.Service
	inventory *inventory.Service
	page      *template.Template
	heroMenu  []order.MenuItem
	logger    *log.Logger
}

// New prepares the template once to respect the proverb "A little copying is better than a little dependency."
func New(orderService *order.Service, inventoryService *inventory.Service, logger *log.Logger) (*Server, error) {
	tmpl, err := template.ParseFS(uiFS, "public_html/app.gohtml")
	if err != nil {
		return nil, err
	}
	if logger == nil {
		// The API defaults to a standard logger so deployments always get feedback about requests.
		logger = log.New(os.Stdout, "[bakery] ", log.LstdFlags)
	}
	return &Server{
		orders:    orderService,
		inventory: inventoryService,
		page:      tmpl,
		heroMenu:  defaultMenu(),
		logger:    logger,
	}, nil
}

// Handler exposes the mux with HTML, JSON, and admin capabilities.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", s.pageHandler("customer"))
	mux.Handle("/admin", s.pageHandler("admin"))
	mux.Handle("/api/orders", s.ordersEndpoint())
	mux.Handle("/api/menu", s.menuEndpoint())
	mux.Handle("/api/admin/inventory", s.inventoryEndpoint())
	return mux
}

// pageHandler renders the single page template with appropriate bootstrapped JSON.
func (s *Server) pageHandler(page string) http.Handler {
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

// ordersEndpoint handles both creation and retrieval to keep JSON endpoints in one place.
func (s *Server) ordersEndpoint() http.Handler {
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

// menuEndpoint exposes the latest menu for both the SPA and admin overlay.
func (s *Server) menuEndpoint() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		items, err := s.inventory.List(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		menu := make([]order.MenuItem, 0, len(items))
		for _, item := range items {
			menu = append(menu, order.MenuItem{
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

// inventoryEndpoint lets bakers manage their batches without exposing raw database handles.
func (s *Server) inventoryEndpoint() http.Handler {
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

// createOrder decodes payloads, performs validation, and hands off work to the channel-powered service.
func (s *Server) createOrder(w http.ResponseWriter, r *http.Request) {
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
		s.logger.Printf("order creation failed: unable to decode payload: %v", err)
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	schedule := make([]order.CroissantSchedule, 0, len(payload.CroissantSchedule))
	for _, slot := range payload.CroissantSchedule {
		if strings.TrimSpace(slot.Day) == "" {
			s.logger.Printf("order creation rejected: missing day for croissant schedule")
			http.Error(w, "day is required", http.StatusBadRequest)
			return
		}
		if slot.Quantity <= 0 {
			s.logger.Printf("order creation rejected: non-positive croissant quantity: %d", slot.Quantity)
			http.Error(w, "quantity must be a positive number", http.StatusBadRequest)
			return
		}
		schedule = append(schedule, order.CroissantSchedule{
			Day:      slot.Day,
			Quantity: slot.Quantity,
			Item:     slot.Item,
		})
	}

	items := make([]order.OrderItem, 0, len(payload.Items))
	for _, item := range payload.Items {
		if strings.TrimSpace(item.Name) == "" {
			s.logger.Printf("order creation rejected: item name missing")
			http.Error(w, "item name is required", http.StatusBadRequest)
			return
		}
		if item.Quantity <= 0 {
			s.logger.Printf("order creation rejected: invalid quantity %d for %s", item.Quantity, item.Name)
			http.Error(w, "item quantity must be positive", http.StatusBadRequest)
			return
		}
		items = append(items, order.OrderItem{
			Name:     item.Name,
			Quantity: item.Quantity,
		})
	}

	request := order.Order{
		CustomerName: payload.Name,
		Phone:        payload.Phone,
		Address:      payload.Address,
		Items:        items,
		BreadSchedule: order.BreadSchedule{
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
		if order.IsValidation(err) {
			s.logger.Printf("order creation failed validation for %s at %s: %v", request.CustomerName, request.Address, err)
			s.respondError(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.logger.Printf("order creation failed for %s at %s: %v", request.CustomerName, request.Address, err)
		s.respondError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.logger.Printf("order stored for %s at %s with %d items", stored.CustomerName, stored.Address, len(stored.Items))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stored)
}

// listOrders returns all collected orders for administrative oversight.
func (s *Server) listOrders(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	orders, err := s.orders.List(ctx)
	if err != nil {
		s.logger.Printf("order listing failed: %v", err)
		s.respondError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.logger.Printf("order listing served with %d records", len(orders))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(orders)
}

// createInventory adds a new baked batch so the front-end menu stays fresh.
func (s *Server) createInventory(w http.ResponseWriter, r *http.Request) {
	var payload inventoryPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		s.logger.Printf("inventory creation failed: unable to decode payload: %v", err)
		s.respondError(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if err := payload.Validate(); err != nil {
		s.logger.Printf("inventory creation rejected: %v", err)
		s.respondError(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	item := inventory.Item{
		Name:           payload.Name,
		Category:       payload.Category,
		BakedAt:        payload.BakedAt,
		PriceCents:     payload.PriceCents,
		AvailableCount: payload.Quantity,
	}
	stored, err := s.inventory.Add(ctx, item)
	if err != nil {
		s.logger.Printf("inventory creation failed for %s: %v", item.Name, err)
		s.respondError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.logger.Printf("inventory item %s recorded with %d units", stored.Name, stored.AvailableCount)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stored)
}

// updateInventory edits an existing batch identified by id.
func (s *Server) updateInventory(w http.ResponseWriter, r *http.Request) {
	var payload inventoryPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		s.logger.Printf("inventory update failed: unable to decode payload: %v", err)
		s.respondError(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if payload.ID == 0 {
		s.logger.Printf("inventory update rejected: missing id")
		s.respondError(w, "id is required", http.StatusBadRequest)
		return
	}
	if err := payload.Validate(); err != nil {
		s.logger.Printf("inventory update rejected for id %d: %v", payload.ID, err)
		s.respondError(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	item := inventory.Item{
		ID:             int64(payload.ID),
		Name:           payload.Name,
		Category:       payload.Category,
		BakedAt:        payload.BakedAt,
		PriceCents:     payload.PriceCents,
		AvailableCount: payload.Quantity,
	}
	if err := s.inventory.Update(ctx, item); err != nil {
		if errors.Is(err, inventory.ErrNotFound) {
			s.logger.Printf("inventory update failed: item %d not found", payload.ID)
			s.respondError(w, err.Error(), http.StatusNotFound)
			return
		}
		s.logger.Printf("inventory update failed for %d: %v", payload.ID, err)
		s.respondError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.logger.Printf("inventory item %d updated with %d units", payload.ID, payload.Quantity)
	w.WriteHeader(http.StatusNoContent)
}

// deleteInventory removes a batch using the query id.
func (s *Server) deleteInventory(w http.ResponseWriter, r *http.Request) {
	rawID := r.URL.Query().Get("id")
	if rawID == "" {
		s.logger.Printf("inventory delete rejected: missing id")
		s.respondError(w, "id is required", http.StatusBadRequest)
		return
	}
	id, err := strconv.Atoi(rawID)
	if err != nil {
		s.logger.Printf("inventory delete rejected: invalid id %s", rawID)
		s.respondError(w, "invalid id", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	if err := s.inventory.Delete(ctx, int64(id)); err != nil {
		if errors.Is(err, inventory.ErrNotFound) {
			s.logger.Printf("inventory delete failed: item %d not found", id)
			s.respondError(w, err.Error(), http.StatusNotFound)
			return
		}
		s.logger.Printf("inventory delete failed for %d: %v", id, err)
		s.respondError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.logger.Printf("inventory item %d deleted", id)
	w.WriteHeader(http.StatusNoContent)
}

// listInventory sends the full inventory for admin controls.
func (s *Server) listInventory(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	items, err := s.inventory.List(ctx)
	if err != nil {
		s.logger.Printf("inventory listing failed: %v", err)
		s.respondError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.logger.Printf("inventory listing served with %d records", len(items))
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

// respondError keeps JSON formatting consistent across endpoints.
func (s *Server) respondError(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// resolveMenu either pulls inventory or falls back to static offerings.
func (s *Server) resolveMenu(ctx context.Context) []order.MenuItem {
	items, err := s.inventory.List(ctx)
	if err != nil || len(items) == 0 {
		return s.heroMenu
	}
	menu := make([]order.MenuItem, 0, len(items))
	for _, item := range items {
		menu = append(menu, order.MenuItem{
			Name:        item.Name,
			Description: fmt.Sprintf("Свежая партия от %s", item.BakedAt.Format("02.01 15:04")),
			Price:       formatPrice(item.PriceCents),
			Image:       imageForCategory(item.Category),
			Category:    item.Category,
		})
	}
	return menu
}

// inventoryPayload keeps transport level parsing separate from core types.
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

// Validate applies parsing to keep HTTP endpoints lean while reporting friendly errors.
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

// inventoryResponse serializes items for the admin table.
type inventoryResponse struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
	BakedAt  string `json:"baked_at"`
	Price    string `json:"price"`
	Quantity int    `json:"quantity"`
}

// defaultMenu showcases signature goods when inventory has no entries.
func defaultMenu() []order.MenuItem {
	return []order.MenuItem{
		{Name: "Сливочный круассан", Description: "Слойки с фермерским маслом", Price: "220 ₽", Image: "classic", Category: "croissant"},
		{Name: "Хрустящий багет", Description: "Пары хватает на утренний стол", Price: "160 ₽", Image: "baguette", Category: "bread"},
		{Name: "Шоколадный десерт", Description: "Горький шоколад 70%", Price: "250 ₽", Image: "chocolate", Category: "pastry"},
	}
}

// imageForCategory chooses icon codes to keep the UI expressive.
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
