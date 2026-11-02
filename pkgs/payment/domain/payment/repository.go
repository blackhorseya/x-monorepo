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
	// ListPendingDue returns up to limit PENDING payments whose window closed at or before now, the longest
	// overdue first.
	ListPendingDue(c context.Context, now time.Time, limit int) ([]*Payment, error)
	// ListReconcilable returns up to limit EXPIRED / CANCELLED payments that have a provider reference and
	// ended at or after notBefore, the ones whose reconcile window closes soonest first.
	ListReconcilable(c context.Context, notBefore time.Time, limit int) ([]*Payment, error)
	// MarkSwept records that the sweep looked at these payments at `at` — asked the provider, whatever the
	// answer, or failed to. It is the sweep's cursor: both lists put the least recently looked-at first, so
	// a payment that stays in the list (not paid, query failing) lets the next one through next run. It is
	// not a state change: no version moves, so it never loses a race against a writer of the payment.
	MarkSwept(c context.Context, ids []ID, at time.Time) error
	// ReconcileBacklog measures the reconcile list: how many are eligible and how long the least recently
	// looked-at has waited.
	ReconcileBacklog(c context.Context, notBefore time.Time) (Backlog, error)
	// Save inserts a never-saved payment (Version 0) or updates a loaded one, comparing Version.
	// ErrStale when another writer won; a second payment for the same order is ErrDuplicateForOrder.
	// The stored version advances on every save, but the in-memory aggregate does not: reload
	// before saving it again.
	Save(c context.Context, p *Payment) error
}

// Backlog is how far behind the reconcile pass is, measured on the payments it still owes a query.
type Backlog struct {
	// Eligible is how many EXPIRED / CANCELLED payments with a checkout are still inside the window.
	Eligible int
	// OldestUnchecked is when the least recently looked-at of them was last looked at (or, never looked
	// at, when it ended). The zero time when nothing is eligible.
	OldestUnchecked time.Time
}

// OutcomeRepository remembers how an order ended before its payment existed (docs/payment.md T15):
// OrderCancelled / OrderCompleted travel on other topics than OrderConfirmed, so they can arrive
// first. Without the note a payment created afterwards would sit PENDING, payable, forever.
type OutcomeRepository interface {
	// Lock serializes the writers of one order for the rest of the surrounding transaction: the creation
	// of its payment (OrderConfirmed) and the recording of how it ended (OrderCancelled / OrderCompleted).
	// Without a payment row there is nothing for a compare-and-set to compare, so each side would decide
	// on what it read before the other committed, and both would ack. Take it before reading anything
	// the decision depends on.
	Lock(c context.Context, orderID string) error
	// Find returns the recorded outcome of orderID, or OutcomeNone when nothing was recorded.
	Find(c context.Context, orderID string) (Outcome, error)
	// Record stores outcome (CANCELLED or COMPLETED_UNPAID) for orderID unless one is already
	// recorded: the first word about an order stands, a repeat changes nothing.
	Record(c context.Context, orderID string, outcome Outcome, at time.Time) error
}
