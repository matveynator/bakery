package inventory

import "time"

// Item captures a single batch baked by the team so the admin interface can track freshness.
type Item struct {
	ID             int64     `json:"id"`
	Name           string    `json:"name"`
	Category       string    `json:"category"`
	AvailableCount int       `json:"available_count"`
	PriceCents     int       `json:"price_cents"`
	BakedAt        time.Time `json:"baked_at"`
	CreatedAt      time.Time `json:"created_at"`
}
