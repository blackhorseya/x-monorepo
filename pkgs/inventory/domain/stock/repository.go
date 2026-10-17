package stock

import "context"

// Repository persists Stock, one row per SKU.
type Repository interface {
	// FindBySKU returns ErrNotFound when the SKU was never stocked.
	FindBySKU(c context.Context, sku SKU) (*Stock, error)
	// Save inserts a never-saved stock (Version 0) or updates a loaded one,
	// comparing Version. ErrStale when another writer won, including a second
	// insert of the same SKU. The stored version advances on every save, but
	// the in-memory aggregate does not: reload before saving it again.
	Save(c context.Context, s *Stock) error
}
