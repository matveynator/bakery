package httpserver

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"bakery/internal/inventory"
	"bakery/internal/order"
)

//go:embed templates/*
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Server provides the HTTP wiring between the web layer and the asynchronous domain service.
type Server struct {
	orders    *order.Service
	inventory *inventory.Service
	templates *template.Template
	heroMenu  []order.MenuItem
}

// New builds the server with parsed templates so each request only executes fast template execution.
func New(orderService *order.Service, inventoryService *inventory.Service) (*Server, error) {
	tmpl, err := template.ParseFS(templateFS, "templates/index.gohtml", "templates/admin.gohtml")
	if err != nil {
		return nil, err
	}
	return &Server{
		orders:    orderService,
		inventory: inventoryService,
		templates: tmpl,
		heroMenu:  defaultMenu(),
	}, nil
}

// Handler returns the mux with all necessary routes and assets.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", s.indexHandler())
	mux.Handle("/admin", s.adminHandler())
	mux.Handle("/api/orders", s.ordersHandler())
	mux.Handle("/api/menu", s.menuHandler())
	mux.Handle("/api/admin/inventory", s.inventoryHandler())
	fileServer := http.FileServer(http.FS(staticFS))
	mux.Handle("/static/", http.StripPrefix("/static/", fileServer))
	return mux
}

// indexHandler renders the landing page with the bakery catalog ready to be controlled by JavaScript.
func (s *Server) indexHandler() http.Handler {
	type pageData struct {
		Menu           []order.MenuItem
		FreeDelivery   string
		CroissantBlurb string
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		menu := s.resolveMenu(r.Context())
		data := pageData{
			Menu:           menu,
			FreeDelivery:   "Бесплатная доставка по району Белая Ромашка каждое утро",
			CroissantBlurb: "Выберите дни и количество для свежих круассанов",
		}
		if err := s.templates.ExecuteTemplate(w, "index.gohtml", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	})
}

// adminHandler renders the lightweight dashboard so bakers can edit inventory quickly.
func (s *Server) adminHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := s.templates.ExecuteTemplate(w, "admin.gohtml", nil); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	})
}

// ordersHandler manages both POST and GET on the /api/orders endpoint.
func (s *Server) ordersHandler() http.Handler {
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

// menuHandler exposes current availability for both the landing page and admin live view.
func (s *Server) menuHandler() http.Handler {
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

// inventoryHandler allows admins to create and manage baked batches.
func (s *Server) inventoryHandler() http.Handler {
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

// createOrder decodes the request, validates the payload, and delegates to the service.
func (s *Server) createOrder(w http.ResponseWriter, r *http.Request) {
	type orderRequest struct {
		Name              string                    `json:"name"`
		Address           string                    `json:"address"`
		Phone             string                    `json:"phone"`
		Comment           string                    `json:"comment"`
		Items             []order.OrderItem         `json:"items"`
		BreadSchedule     order.BreadSchedule       `json:"bread_schedule"`
		CroissantSchedule []order.CroissantSchedule `json:"croissant_schedule"`
	}

	var req orderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	orderModel := order.Order{
		CustomerName:      strings.TrimSpace(req.Name),
		Address:           strings.TrimSpace(req.Address),
		Phone:             strings.TrimSpace(req.Phone),
		Comment:           strings.TrimSpace(req.Comment),
		Items:             req.Items,
		BreadSchedule:     req.BreadSchedule,
		CroissantSchedule: req.CroissantSchedule,
	}

	stored, err := s.orders.Submit(ctx, orderModel)
	if err != nil {
		status := http.StatusInternalServerError
		if order.IsValidation(err) {
			status = http.StatusBadRequest
		}
		http.Error(w, err.Error(), status)
		return
	}

	response := map[string]interface{}{
		"order_id": stored.ID,
		"message":  "Спасибо! Мы доставим свежую выпечку утром по графику",
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// listOrders returns the current orders for potential admin integrations or manual verification.
func (s *Server) listOrders(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	orders, err := s.orders.List(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(orders)
}

// listInventory lets the admin refresh without reloading the page.
func (s *Server) listInventory(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	items, err := s.inventory.List(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(items)
}

// createInventory stores a new batch entry coming from the admin form.
func (s *Server) createInventory(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Name           string `json:"name"`
		Category       string `json:"category"`
		AvailableCount int    `json:"available_count"`
		PriceCents     int    `json:"price_cents"`
		BakedAt        string `json:"baked_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	bakedAt, err := time.Parse(time.RFC3339, payload.BakedAt)
	if err != nil {
		http.Error(w, "invalid baked_at format", http.StatusBadRequest)
		return
	}
	item := inventory.Item{
		Name:           strings.TrimSpace(payload.Name),
		Category:       strings.TrimSpace(payload.Category),
		AvailableCount: payload.AvailableCount,
		PriceCents:     payload.PriceCents,
		BakedAt:        bakedAt,
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	stored, err := s.inventory.Add(ctx, item)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stored)
}

// updateInventory edits counts or prices for an existing batch.
func (s *Server) updateInventory(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		ID             int64  `json:"id"`
		AvailableCount int    `json:"available_count"`
		PriceCents     int    `json:"price_cents"`
		BakedAt        string `json:"baked_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	bakedAt, err := time.Parse(time.RFC3339, payload.BakedAt)
	if err != nil {
		http.Error(w, "invalid baked_at format", http.StatusBadRequest)
		return
	}
	item := inventory.Item{
		ID:             payload.ID,
		AvailableCount: payload.AvailableCount,
		PriceCents:     payload.PriceCents,
		BakedAt:        bakedAt,
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.inventory.Update(ctx, item); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteInventory removes a batch entirely.
func (s *Server) deleteInventory(w http.ResponseWriter, r *http.Request) {
	idParam := r.URL.Query().Get("id")
	if idParam == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(idParam, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.inventory.Delete(ctx, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// resolveMenu tries the admin-managed inventory first and falls back to the hero menu for inspiration.
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

// imageForCategory keeps icon selection centralized.
func imageForCategory(category string) string {
	switch strings.ToLower(category) {
	case "bread":
		return "/static/images/sourdough.svg"
	case "baguette":
		return "/static/images/baguette.svg"
	case "ciabatta":
		return "/static/images/ciabatta.svg"
	case "chocolate":
		return "/static/images/chocolate.svg"
	case "almond":
		return "/static/images/almond.svg"
	case "classic":
		return "/static/images/classic.svg"
	default:
		return "/static/images/hero.svg"
	}
}

// formatPrice renders kopeck values in a human readable string.
func formatPrice(cents int) string {
	rub := cents / 100
	rem := cents % 100
	if rem == 0 {
		return fmt.Sprintf("%d₽", rub)
	}
	if rem < 0 {
		rem = -rem
	}
	return fmt.Sprintf("%d,%02d₽", rub, rem)
}

// defaultMenu keeps the product catalog centralized for the server and template.
func defaultMenu() []order.MenuItem {
	return []order.MenuItem{
		{Name: "Миндальный круассан", Description: "Слоеное тесто с миндальным кремом", Price: "240₽", Image: "/static/images/almond.svg", Category: "almond"},
		{Name: "Классический круассан", Description: "Хрустящая классика", Price: "190₽", Image: "/static/images/classic.svg", Category: "classic"},
		{Name: "Шоколадный круассан", Description: "Темный шоколад внутри", Price: "220₽", Image: "/static/images/chocolate.svg", Category: "chocolate"},
		{Name: "Заквасочный хлеб", Description: "Французский заквасочный батон", Price: "260₽", Image: "/static/images/sourdough.svg", Category: "bread"},
		{Name: "Багет", Description: "Классический багет", Price: "150₽", Image: "/static/images/baguette.svg", Category: "baguette"},
		{Name: "Чиабатта", Description: "Оливковое масло и травы", Price: "210₽", Image: "/static/images/ciabatta.svg", Category: "ciabatta"},
	}
}
