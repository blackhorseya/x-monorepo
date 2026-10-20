package stock

import "context"

// ListParams pages a listing ascending by SKU. Limit is already clamped by
// the caller; After is "" for the first page.
type ListParams struct {
	Limit int
	After SKU
}

// Repository persists Stock, one row per SKU.
type Repository interface {
	// FindBySKU returns ErrNotFound when the SKU was never stocked.
	FindBySKU(c context.Context, sku SKU) (*Stock, error)
	// Save inserts a never-saved stock (Version 0) or updates a loaded one,
	// comparing Version. ErrStale when another writer won, including a second
	// insert of the same SKU. The stored version advances on every save, but
	// the in-memory aggregate does not: reload before saving it again.
	Save(c context.Context, s *Stock) error
	// List returns up to p.Limit stocks with SKU > p.After, ascending, and
	// the SKU to continue from, or "" when this is the last page.
	List(c context.Context, p ListParams) ([]*Stock, SKU, error)
}
