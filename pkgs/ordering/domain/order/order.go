// Package order is the Order aggregate of the ordering bounded context.
// It imports only the shared kernel: no application, infra, adapters, or contract (proto).
package order

import (
	"slices"
	"strings"
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
	// paymentRequired is the snapshot of ordering.require_payment taken when the order became
	// CONFIRMED and never changed after (docs/payment.md §2). false before CONFIRMED and for every
	// order that was CONFIRMED before the setting existed.
	paymentRequired bool
	// version is the optimistic-concurrency token Repository.Save compares:
	// 0 for an order never saved, then the number of saves.
	version int64
	// idempotencyKey / requestHash are set only on an order created with a key:
	// the caller's retry token and the fingerprint of the request it was spent on
	// (FingerprintV2; orders from before server pricing hold the v1 Fingerprint).
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
		o.requestHash = FingerprintV2(requestLines(o.items))
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
	// PaymentRequired is the stored snapshot (see Order.paymentRequired).
	PaymentRequired bool
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

		paymentRequired: p.PaymentRequired,
		idempotencyKey:  p.IdempotencyKey,
		requestHash:     p.RequestHash,
	}, nil
}

// Confirm moves the order to Confirmed: inventory reserved every line. A late
// StockReserved for an order already cancelled is refused (ErrCannotConfirm);
// inventory releases that reservation on the OrderCancelled it also receives.
// requirePayment is ordering.require_payment as it is at this moment; it is stored as the
// order's payment_required snapshot and decides everything later (docs/payment.md §2).
func (x *Order) Confirm(now time.Time, requirePayment bool) error {
	if !x.status.CanTransitionTo(StatusConfirmed) {
		return ErrCannotConfirm
	}

	from := x.status
	x.status = StatusConfirmed
	x.paymentRequired = requirePayment
	x.updatedAt = now
	x.record(NewConfirmed(x, from))

	return nil
}

// Complete fulfils a Confirmed or Paid order: the stock inventory held for it is sold.
// Driven by the rpc adapter; inventory commits the reservation on the
// OrderCompleted this raises. A completed order cannot be cancelled afterwards.
//
// A CONFIRMED order whose snapshot says payment is required is refused with
// ErrPaymentRequired: it only moves on through MarkPaid. The status check comes first, so
// COMPLETED and CANCELLED orders keep answering ErrCannotComplete (docs/payment.md §8).
func (x *Order) Complete(now time.Time) error {
	if !x.status.CanTransitionTo(StatusCompleted) {
		return ErrCannotComplete
	}
	if x.status == StatusConfirmed && x.paymentRequired {
		return ErrPaymentRequired
	}

	x.complete(now, CauseRPCRequest) // only the rpc completes an order by hand

	return nil
}

// complete is the transition itself, for callers that have already checked it may happen.
func (x *Order) complete(now time.Time, cause Cause) {
	from := x.status
	x.status = StatusCompleted
	x.updatedAt = now
	x.record(NewCompleted(x, from, cause))
}

// MarkPaid records the payment of a Confirmed order (CONFIRMED → PAID). Nothing calls it in
// slice 1: the contract exists so that Payment (slice 3) has a state to move to. A paid order
// cannot be cancelled.
func (x *Order) MarkPaid(now time.Time) error {
	if !x.status.CanTransitionTo(StatusPaid) {
		return ErrCannotPay
	}

	x.markPaid(now, CausePaymentCaptured)

	return nil
}

// markPaid is the transition itself, for callers that have already checked it may happen.
func (x *Order) markPaid(now time.Time, cause Cause) {
	from := x.status
	x.status = StatusPaid
	x.updatedAt = now
	x.record(NewPaid(x, from, cause))
}

// Cancel moves the order to Cancelled if the state machine allows it. The caller says why: the
// aggregate cannot tell a client's request from another BC's (CauseRPCRequest, or
// CauseCancellationRequested / CauseStockShortage / CauseReservationExpired for another BC's, the
// last two when it classified its request); anything else, the zero value included, is ErrInvalidCause and
// changes nothing. The state machine's refusal comes first.
func (x *Order) Cancel(now time.Time, cause Cause) error {
	if !x.status.CanTransitionTo(StatusCancelled) {
		return ErrCannotCancel
	}
	if !cause.validForCancel() {
		return ErrInvalidCause
	}

	x.cancel(now, cause)

	return nil
}

// cancel is the transition itself, for callers that have already checked it may happen.
func (x *Order) cancel(now time.Time, cause Cause) {
	from := x.status
	x.status = StatusCancelled
	x.updatedAt = now
	x.record(NewCancelled(x, from, cause))
}

func (x *Order) ID() ID               { return x.id }
func (x *Order) CustomerID() string   { return x.customerID }
func (x *Order) Items() []LineItem    { return slices.Clone(x.items) }
func (x *Order) Total() kernel.Money  { return x.total }
func (x *Order) Status() Status       { return x.status }
func (x *Order) CreatedAt() time.Time { return x.createdAt }
func (x *Order) UpdatedAt() time.Time { return x.updatedAt }
func (x *Order) Version() int64       { return x.version }

// PaymentRequired is the snapshot taken when the order became CONFIRMED (see the field).
func (x *Order) PaymentRequired() bool { return x.paymentRequired }

// IdempotencyKey is the retry token the order was created with, "" if none.
func (x *Order) IdempotencyKey() string { return x.idempotencyKey }

// RequestHash is the fingerprint of the request the idempotency key was spent on, "" if no key.
func (x *Order) RequestHash() string { return x.requestHash }

// IsReplayOf reports whether lines are the request this order's idempotency key was first
// spent on. An order without a key replays nothing.
//
// A v2 hash covers sku and quantity, so it is compared with the lines directly. A v1 hash
// (an order placed when the caller typed the price) also covers the price, which a retry no
// longer carries: the lines are priced with the snapshot on this order, position by
// position, and compared as v1. That is the same answer the retry always got — same
// sku / quantity sequence replays, anything else is a reused key — without asking the
// catalog, whose price may have moved since.
func (x *Order) IsReplayOf(lines []RequestLine) bool {
	switch {
	case x.requestHash == "":
		return false
	case strings.HasPrefix(x.requestHash, fingerprintV2Prefix):
		return x.requestHash == FingerprintV2(lines)
	}

	if len(lines) != len(x.items) {
		return false
	}
	asPlaced := make([]LineItem, 0, len(lines))
	for n, l := range lines {
		if l.sku != x.items[n].sku {
			return false
		}
		asPlaced = append(asPlaced, LineItem{sku: l.sku, quantity: l.quantity, unitPrice: x.items[n].unitPrice})
	}

	return x.requestHash == Fingerprint(asPlaced)
}

// sumItems totals the lines. They share one currency: each product has its own, an order
// has one.
func sumItems(items []LineItem) (kernel.Money, error) {
	if len(items) == 0 {
		return kernel.Money{}, ErrNoItems
	}

	total := kernel.ZeroMoney(items[0].UnitPrice().Currency())
	for _, it := range items {
		var err error
		if total, err = total.Add(it.Subtotal()); err != nil {
			return kernel.Money{}, ErrMixedCurrency // Add fails only on a currency mismatch
		}
	}

	return total, nil
}
