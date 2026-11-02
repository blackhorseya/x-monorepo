package payment

import (
	"context"
	"time"
)

// Repository persists Payment, one row per order.
type Repository interface {
	// FindByOrderID returns ErrNotFound when the order has no payment.
	FindByOrderID(c context.Context, orderID string) (*Payment, error)
	// FindByID returns ErrNotFound when no payment has this id.
	FindByID(c context.Context, id ID) (*Payment, error)
	// FindByProviderRef returns ErrNotFound when no payment was opened at the provider under ref.
	FindByProviderRef(c context.Context, provider, ref string) (*Payment, error)
	// Save inserts a never-saved payment (Version 0) or updates a loaded one, comparing Version.
	// ErrStale when another writer won; a second payment for the same order is ErrDuplicateForOrder.
	// The stored version advances on every save, but the in-memory aggregate does not: reload
	// before saving it again.
	Save(c context.Context, p *Payment) error
}

// OutcomeRepository remembers how an order ended before its payment existed (docs/payment.md T15):
// OrderCancelled / OrderCompleted travel on other topics than OrderConfirmed, so they can arrive
// first. Without the note a payment created afterwards would sit PENDING, payable, forever.
type OutcomeRepository interface {
	// Find returns the recorded outcome of orderID, or OutcomeNone when nothing was recorded.
	Find(c context.Context, orderID string) (Outcome, error)
	// Record stores outcome (CANCELLED or COMPLETED_UNPAID) for orderID unless one is already
	// recorded: the first word about an order stands, a repeat changes nothing.
	Record(c context.Context, orderID string, outcome Outcome, at time.Time) error
}
