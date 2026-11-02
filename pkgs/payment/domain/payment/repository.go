package payment

import "context"

// Repository persists Payment, one row per order.
type Repository interface {
	// FindByOrderID returns ErrNotFound when the order has no payment.
	FindByOrderID(c context.Context, orderID string) (*Payment, error)
	// Save inserts a never-saved payment (Version 0) or updates a loaded one, comparing Version.
	// ErrStale when another writer won; a second payment for the same order is ErrDuplicateForOrder.
	// The stored version advances on every save, but the in-memory aggregate does not: reload
	// before saving it again.
	Save(c context.Context, p *Payment) error
}
