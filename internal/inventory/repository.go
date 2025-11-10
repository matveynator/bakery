package inventory

import (
	"context"
	"database/sql"
	"time"
)

// Repository persists items through database/sql so storage backends stay swappable.
type Repository struct {
	db *sql.DB
}

// NewRepository wires the handle so goroutines can work without sharing mutable state.
func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

// Save inserts a freshly baked batch so the storefront can expose it immediately.
func (r *Repository) Save(ctx context.Context, item Item) (Item, error) {
	query := "INSERT INTO inventory (name, category, available_count, price_cents, baked_at) VALUES (?, ?, ?, ?, ?)"
	result, err := r.db.ExecContext(ctx, query, item.Name, item.Category, item.AvailableCount, item.PriceCents, item.BakedAt.UTC())
	if err != nil {
		return Item{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Item{}, err
	}
	item.ID = id
	item.CreatedAt = time.Now().UTC()
	return item, nil
}

// List fetches every batch so both the admin and the landing page stay in sync.
func (r *Repository) List(ctx context.Context) ([]Item, error) {
	query := "SELECT id, name, category, available_count, price_cents, baked_at FROM inventory ORDER BY baked_at DESC"
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		var item Item
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

// Update refreshes count and pricing so the admin can reflect sold or discounted goods quickly.
func (r *Repository) Update(ctx context.Context, item Item) error {
	query := "UPDATE inventory SET available_count = ?, price_cents = ?, baked_at = ? WHERE id = ?"
	_, err := r.db.ExecContext(ctx, query, item.AvailableCount, item.PriceCents, item.BakedAt.UTC(), item.ID)
	return err
}

// Delete removes a batch entirely which is handy once everything is sold out.
func (r *Repository) Delete(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM inventory WHERE id = ?", id)
	return err
}
