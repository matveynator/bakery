package inventory

import "errors"

// ErrNotFound is returned when an item is missing so HTTP handlers can respond with 404.
var ErrNotFound = errors.New("inventory item not found")
