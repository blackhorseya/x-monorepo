package stock

import "context"

// Repository persists Stock, one row per SKU.
type Repository interface {
	// FindBySKU returns ErrNotFound when the SKU was never stocked.
	FindBySKU(c context.Context, sku SKU) (*Stock, error)
	// Save inserts or updates, comparing Version: ErrStale when another writer
	// won. The stored version advances on every successful update.
	Save(c context.Context, s *Stock) error
}
