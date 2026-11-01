package stock

import "context"

// ListParams pages a listing ascending by SKU. Limit is already clamped by
// the caller; After is "" for the first page. SKUPrefix "" matches every
// SKU; OnlyReserved keeps the SKUs with reserved > 0.
type ListParams struct {
	Limit        int
	After        SKU
	SKUPrefix    string
	OnlyReserved bool
}

// Repository persists Stock, one row per SKU.
type Repository interface {
	// FindBySKU returns ErrNotFound when the SKU was never stocked.
	FindBySKU(c context.Context, sku SKU) (*Stock, error)
	// FindBySKUs returns the stocks of the given SKUs in one read, in no
	// particular order. A SKU that was never stocked is left out, not an
	// error; duplicates in skus are read once.
	FindBySKUs(c context.Context, skus []SKU) ([]*Stock, error)
	// Save inserts a never-saved stock (Version 0) or updates a loaded one,
	// comparing Version. ErrStale when another writer won, including a second
	// insert of the same SKU. The stored version advances on every save, but
	// the in-memory aggregate does not: reload before saving it again.
	Save(c context.Context, s *Stock) error
	// List returns up to p.Limit stocks matching p's filters with SKU >
	// p.After, ascending, and the SKU to continue from, or "" when this is
	// the last page.
	List(c context.Context, p ListParams) ([]*Stock, SKU, error)
}
