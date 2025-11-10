package order

import "time"

// OrderItem describes a single product and the quantity requested.
type OrderItem struct {
	Name     string `json:"name"`
	Quantity int    `json:"quantity"`
}

// BreadSchedule expresses how often the household expects loaves in the morning delivery.
type BreadSchedule struct {
	Days      []string `json:"days"`
	Frequency string   `json:"frequency"`
	StartDate string   `json:"start_date"`
	Notes     string   `json:"notes"`
}

// CroissantSchedule lists the weekday and amount per drop so batching stays predictable.
type CroissantSchedule struct {
	Day      string `json:"day"`
	Quantity int    `json:"quantity"`
	Item     string `json:"item"`
}

// Order aggregates all information required to deliver bakery goods around the district.
type Order struct {
	ID                int64
	CustomerName      string
	Address           string
	Phone             string
	Items             []OrderItem
	BreadSchedule     BreadSchedule
	CroissantSchedule []CroissantSchedule
	Comment           string
	CreatedAt         time.Time
}

// MenuItem is used to render the catalog on the landing page.
type MenuItem struct {
	Name        string
	Description string
	Price       string
	Image       string
	Category    string
}
