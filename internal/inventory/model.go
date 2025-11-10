package inventory

import "time"

// Item captures a single batch baked by the team so the admin interface can track freshness.
type Item struct {
	ID             int64
	Name           string
	Category       string
	AvailableCount int
	PriceCents     int
	BakedAt        time.Time
	CreatedAt      time.Time
}
