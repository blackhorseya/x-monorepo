package order

import "context"

// ListParams narrows and pages a listing. Limit is already clamped by the
// caller; After is nil for the first page.
type ListParams struct {
	CustomerID string  // "" = any
	Status     *Status // nil = any
	Limit      int
	After      *Cursor
}

// Repository persists Order aggregates. Implementations live in ordering/infra.
type Repository interface {
	// FindByID returns ErrNotFound when the order does not exist.
	FindByID(c context.Context, id ID) (*Order, error)
	// FindByIDForCustomer is FindByID restricted to one customer's orders: an
	// order of somebody else is ErrNotFound, decided where the row is selected
	// (WHERE id AND customer_id), so a foreign order costs what a missing one
	// costs — its items are never loaded.
	FindByIDForCustomer(c context.Context, id ID, customerID string) (*Order, error)
	// FindByIdempotencyKey returns the customer's order created with key, or
	// ErrNotFound. The scope is (customer, key): another customer's key is not visible.
	FindByIdempotencyKey(c context.Context, customerID, key string) (*Order, error)
	// Save inserts a new order (Version 0; ErrDuplicate if the id is taken,
	// ErrIdempotencyConflict if it carries a key the customer already used —
	// nothing is written then, and the surrounding transaction stays usable) or
	// updates a loaded one, comparing Version: ErrStale when another writer saved
	// it first. The stored version advances on every save, but the in-memory
	// aggregate does not: reload before saving it again.
	Save(c context.Context, o *Order) error
	// List returns up to p.Limit orders newest first (created_at desc, id desc)
	// and the cursor of the next page, or nil when this is the last page.
	List(c context.Context, p ListParams) ([]*Order, *Cursor, error)
	// CountByStatus returns one StatusCount per status that has at least one
	// order (none for an empty status): the funnel the worker exports as gauges.
	CountByStatus(c context.Context) ([]StatusCount, error)
}
