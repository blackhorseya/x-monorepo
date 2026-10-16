package order

import "context"

// Repository persists Order aggregates. Implementations live in ordering/infra.
type Repository interface {
	// FindByID returns ErrNotFound when the order does not exist.
	FindByID(c context.Context, id ID) (*Order, error)
	// Save inserts or updates the whole aggregate.
	Save(c context.Context, o *Order) error
}
