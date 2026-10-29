package reservation

import (
	"context"
	"time"
)

// Repository persists Reservation, one per order. Add and Update are
// separate so a race on the same order (two workers, or OrderCreated against
// an overtaking OrderCancelled, both found nothing) ends in ErrDuplicate for
// the loser instead of a silent overwrite; the use cases turn that into a
// retry that lands on the winner's row.
type Repository interface {
	// FindByOrderID returns ErrNotFound when the order has no reservation yet.
	FindByOrderID(c context.Context, orderID string) (*Reservation, error)
	// FindByOrderIDs returns the reservations that exist for the given orders,
	// once each and in no particular order; an order without one is skipped.
	FindByOrderIDs(c context.Context, orderIDs []string) ([]*Reservation, error)
	// Add inserts a new reservation; ErrDuplicate when the order already has one.
	Add(c context.Context, r *Reservation) error
	// Update stores a status change; ErrNotFound when it was never added and
	// ErrStale when the stored status is no longer r.LoadedStatus() (compare-and-swap).
	Update(c context.Context, r *Reservation) error
	// List returns up to p.Limit reservations newest first (created_at desc,
	// order_id desc) and the cursor of the next page, or nil when this is the
	// last page.
	List(c context.Context, p ListParams) ([]*Reservation, *Cursor, error)
	// CountByStatus returns one StatusCount per status that has at least one
	// reservation (none for an empty status): the funnel the worker exports as gauges.
	CountByStatus(c context.Context) ([]StatusCount, error)
	// ListReservedBefore returns up to limit RESERVED reservations whose hold
	// started (updated_at) before the instant, oldest first: the expiry sweep's input.
	ListReservedBefore(c context.Context, before time.Time, limit int) ([]*Reservation, error)
}
