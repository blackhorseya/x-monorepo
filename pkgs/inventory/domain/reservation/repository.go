package reservation

import "context"

// Repository persists Reservation, one per order. Add and Update are
// separate so a race on the same order (two workers, or OrderCreated against
// an overtaking OrderCancelled, both found nothing) ends in ErrDuplicate for
// the loser instead of a silent overwrite; the use cases turn that into a
// retry that lands on the winner's row.
type Repository interface {
	// FindByOrderID returns ErrNotFound when the order has no reservation yet.
	FindByOrderID(c context.Context, orderID string) (*Reservation, error)
	// Add inserts a new reservation; ErrDuplicate when the order already has one.
	Add(c context.Context, r *Reservation) error
	// Update stores a status change; ErrNotFound when it was never added.
	Update(c context.Context, r *Reservation) error
}
