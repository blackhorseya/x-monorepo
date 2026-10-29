// Package order is the Order aggregate of the ordering bounded context.
// It imports only the shared kernel: no application, infra, adapters, or contract (proto).
package order

import (
	"slices"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

// ID identifies an Order.
type ID string

func (x ID) String() string { return string(x) }

// Order is the aggregate root. Fields are private; state changes go through behaviour methods.
type Order struct {
	id         ID
	customerID string
	items      []LineItem
	total      kernel.Money
	status     Status
	createdAt  time.Time
	updatedAt  time.Time
	// version is the optimistic-concurrency token Repository.Save compares:
	// 0 for an order never saved, then the number of saves.
	version int64
	// idempotencyKey / requestHash are set only on an order created with a key:
	// the caller's retry token and the Fingerprint of the items it was spent on.
	idempotencyKey string
	requestHash    string
	events         []Event // raised, not yet pulled (see events.go)
}

// Params are the inputs of New. A parameter object (not state: every field is exported
// and it is consumed once) keeps call sites readable and lets fields be added without
// touching every caller.
type Params struct {
	ID         ID
	CustomerID string
	Items      []LineItem
	Now        time.Time
	// IdempotencyKey is optional; "" = the order is not retry-protected.
	IdempotencyKey string
}

// New creates a pending Order and enforces creation invariants.
func New(p Params) (*Order, error) {
	if p.ID == "" {
		return nil, ErrInvalidID
	}
	if p.CustomerID == "" {
		return nil, ErrEmptyCustomer
	}

	total, err := sumItems(p.Items)
	if err != nil {
		return nil, err
	}

	o := &Order{
		id:         p.ID,
		customerID: p.CustomerID,
		items:      slices.Clone(p.Items),
		total:      total,
		status:     StatusPending,
		createdAt:  p.Now,
		updatedAt:  p.Now,
	}
	if p.IdempotencyKey != "" {
		o.idempotencyKey = p.IdempotencyKey
		o.requestHash = Fingerprint(o.items)
	}
	o.record(NewCreated(o))

	return o, nil
}

// ReconstituteParams carry persisted state back into the aggregate, as stored.
type ReconstituteParams struct {
	ID         ID
	CustomerID string
	Items      []LineItem
	Status     Status
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Version    int64
	// IdempotencyKey / RequestHash are "" for an order created without a key.
	IdempotencyKey string
	RequestHash    string
}

// Reconstitute rebuilds an Order from persistence. Only repositories should call it.
func Reconstitute(p ReconstituteParams) (*Order, error) {
	total, err := sumItems(p.Items)
	if err != nil {
		return nil, err
	}

	return &Order{
		id:         p.ID,
		customerID: p.CustomerID,
		items:      slices.Clone(p.Items),
		total:      total,
		status:     p.Status,
		createdAt:  p.CreatedAt,
		updatedAt:  p.UpdatedAt,
		version:    p.Version,

		idempotencyKey: p.IdempotencyKey,
		requestHash:    p.RequestHash,
	}, nil
}

// Confirm moves the order to Confirmed: inventory reserved every line. A late
// StockReserved for an order already cancelled is refused (ErrCannotConfirm);
// inventory releases that reservation on the OrderCancelled it also receives.
func (x *Order) Confirm(now time.Time) error {
	if !x.status.CanTransitionTo(StatusConfirmed) {
		return ErrCannotConfirm
	}

	x.status = StatusConfirmed
	x.updatedAt = now
	x.record(NewConfirmed(x))

	return nil
}

// Complete fulfils a Confirmed order: the stock inventory held for it is sold.
// Driven by the rpc adapter; inventory commits the reservation on the
// OrderCompleted this raises. A completed order cannot be cancelled afterwards.
func (x *Order) Complete(now time.Time) error {
	if !x.status.CanTransitionTo(StatusCompleted) {
		return ErrCannotComplete
	}

	x.status = StatusCompleted
	x.updatedAt = now
	x.record(NewCompleted(x))

	return nil
}

// Cancel moves the order to Cancelled if the state machine allows it.
func (x *Order) Cancel(now time.Time) error {
	if !x.status.CanTransitionTo(StatusCancelled) {
		return ErrCannotCancel
	}

	x.status = StatusCancelled
	x.updatedAt = now
	x.record(NewCancelled(x))

	return nil
}

func (x *Order) ID() ID               { return x.id }
func (x *Order) CustomerID() string   { return x.customerID }
func (x *Order) Items() []LineItem    { return slices.Clone(x.items) }
func (x *Order) Total() kernel.Money  { return x.total }
func (x *Order) Status() Status       { return x.status }
func (x *Order) CreatedAt() time.Time { return x.createdAt }
func (x *Order) UpdatedAt() time.Time { return x.updatedAt }
func (x *Order) Version() int64       { return x.version }

// IdempotencyKey is the retry token the order was created with, "" if none.
func (x *Order) IdempotencyKey() string { return x.idempotencyKey }

// RequestHash is the Fingerprint of the items the idempotency key was spent on, "" if no key.
func (x *Order) RequestHash() string { return x.requestHash }

// IsReplayOf reports whether items are the request this order's idempotency key
// was first spent on (same Fingerprint). An order without a key replays nothing.
func (x *Order) IsReplayOf(items []LineItem) bool {
	return x.requestHash != "" && x.requestHash == Fingerprint(items)
}

func sumItems(items []LineItem) (kernel.Money, error) {
	if len(items) == 0 {
		return kernel.Money{}, ErrNoItems
	}

	total := kernel.ZeroMoney(items[0].UnitPrice().Currency())
	for _, it := range items {
		var err error
		if total, err = total.Add(it.Subtotal()); err != nil {
			return kernel.Money{}, err
		}
	}

	return total, nil
}
