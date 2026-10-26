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
	// Save inserts a new order (Version 0; ErrDuplicate if the id is taken) or
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
