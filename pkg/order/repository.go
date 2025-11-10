package order

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// Repository coordinates the persistence of orders through database/sql so the service stays storage-agnostic.
type Repository struct {
	db *sql.DB
}

// NewRepository wires the database handle so calls can be fanned out from background goroutines.
func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

// Save inserts a new order into the database while delegating serialization details to this layer.
func (r *Repository) Save(ctx context.Context, order Order) (Order, error) {
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

// List fetches all orders to support administrative views or dashboards if needed.
func (r *Repository) List(ctx context.Context) ([]Order, error) {
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
