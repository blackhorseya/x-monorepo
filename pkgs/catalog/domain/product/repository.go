package product

import "context"

// ListParams pages a listing ascending by SKU. Limit is already clamped by
// the caller (0 = no limit); After is "" for the first page. SKUPrefix ""
// matches every SKU.
type ListParams struct {
	Limit     int
	After     SKU
	SKUPrefix string
}

// Repository persists Product, one row per SKU.
type Repository interface {
	// FindBySKU returns ErrNotFound when the SKU was never saved.
	FindBySKU(c context.Context, sku SKU) (*Product, error)
	// FindBySKUs loads many products in one round trip. A SKU that does not
	// exist is absent from the result (not an error); inactive products are
	// included.
	FindBySKUs(c context.Context, skus []SKU) (map[SKU]*Product, error)
	// Save inserts a never-saved product (Version 0) or updates a loaded one,
	// comparing Version. ErrStale when another writer won, including a second
	// insert of the same SKU. The stored version advances on every save, but
	// the in-memory aggregate does not: reload before saving it again.
	Save(c context.Context, p *Product) error
	// List returns up to p.Limit products matching p's filters with SKU >
	// p.After, ascending, and the SKU to continue from, or "" when this is
	// the last page.
	List(c context.Context, p ListParams) ([]*Product, SKU, error)
}
