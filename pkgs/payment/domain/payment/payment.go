// Package payment is the Payment aggregate of the payment bounded context: the single payment of
// one order, and the state machine of docs/payment.md §3. It imports only the shared kernel.
//
// Every transition of the state × event table is a method here, so the table is decided in one
// place and tested cell by cell. A method changes the aggregate and returns an Effect: what the
// application still owes besides saving it (events to publish, an alert, a checkout to cancel).
package payment

import (
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

// ID identifies a Payment. It is also the idempotency key of the provider's refund call.
type ID string

func (x ID) String() string { return string(x) }

// Payment is the aggregate root. version is the optimistic-concurrency token repositories compare
// on Save: 0 for a payment never saved, then the number of saves.
type Payment struct {
	id         ID
	orderID    string
	customerID string
	amount     kernel.Money
	status     Status
	expiresAt  time.Time

	provider          string
	providerRef       string
	checkoutURL       string
	checkoutExpiresAt time.Time

	outcome    Outcome
	terminalAt time.Time

	version   int64
	updatedAt time.Time
}

// NewParams is what OrderConfirmed carries (T1): the order, its customer and total, and when the
// payment window closes (decided by the caller from the configured window).
type NewParams struct {
	ID         ID
	OrderID    string
	CustomerID string
	// Total is the order's total. The zero Money (no currency) means the event carried none; that is
	// ErrMissingTotal, never a payment of zero.
	Total     kernel.Money
	ExpiresAt time.Time
}

func (p NewParams) validate(now time.Time) error {
	switch {
	case p.ID == "":
		return ErrInvalidID
	case p.OrderID == "":
		return ErrInvalidOrderID
	case p.CustomerID == "":
		return ErrEmptyCustomer
	case p.Total == (kernel.Money{}):
		return ErrMissingTotal
	}
	if err := validateAmount(p.Total); err != nil {
		return err
	}
	if !p.ExpiresAt.After(now) {
		return ErrInvalidExpiry
	}

	return nil
}

// New creates a PENDING payment (T1). The provider is not asked: that happens at StartPayment.
func New(p NewParams, now time.Time) (*Payment, error) {
	if err := p.validate(now); err != nil {
		return nil, err
	}

	return &Payment{
		id: p.ID, orderID: p.OrderID, customerID: p.CustomerID, amount: p.Total,
		status: StatusPending, expiresAt: p.ExpiresAt, outcome: OutcomeNone, updatedAt: now,
	}, nil
}

// NewForEndedOrder creates a CANCELLED payment (T14): the order's OrderCancelled or OrderCompleted
// arrived before its OrderConfirmed, so there is nothing left to collect and no timer runs.
func NewForEndedOrder(p NewParams, outcome Outcome, now time.Time) (*Payment, error) {
	if outcome != OutcomeCancelled && outcome != OutcomeCompletedUnpaid {
		return nil, ErrInvalidOutcome
	}
	x, err := New(p, now)
	if err != nil {
		return nil, err
	}
	x.status, x.outcome, x.terminalAt = StatusCancelled, outcome, now

	return x, nil
}

// ReconstituteParams carry persisted state back into the aggregate, as stored.
type ReconstituteParams struct {
	ID                ID
	OrderID           string
	CustomerID        string
	Amount            kernel.Money
	Status            Status
	ExpiresAt         time.Time
	Provider          string
	ProviderRef       string
	CheckoutURL       string
	CheckoutExpiresAt time.Time
	Outcome           Outcome
	TerminalAt        time.Time
	Version           int64
	UpdatedAt         time.Time
}

// Reconstitute rebuilds a Payment from persistence. Only repositories should call it. It refuses
// a combination no transition can produce, so a corrupt row surfaces when it is loaded, not when
// a refund is decided from it.
func Reconstitute(p ReconstituteParams) (*Payment, error) {
	switch {
	case p.ID == "":
		return nil, ErrInvalidID
	case p.OrderID == "":
		return nil, ErrInvalidOrderID
	case p.CustomerID == "":
		return nil, ErrEmptyCustomer
	case !p.Status.valid():
		return nil, ErrInvalidStatus
	case !p.Outcome.valid() || !outcomeFits(p.Status, p.Outcome):
		return nil, ErrInvalidOutcome
	}
	if err := validateAmount(p.Amount); err != nil {
		return nil, err
	}

	return &Payment{
		id: p.ID, orderID: p.OrderID, customerID: p.CustomerID, amount: p.Amount, status: p.Status,
		expiresAt: p.ExpiresAt, provider: p.Provider, providerRef: p.ProviderRef, checkoutURL: p.CheckoutURL,
		checkoutExpiresAt: p.CheckoutExpiresAt, outcome: p.Outcome, terminalAt: p.TerminalAt,
		version: p.Version, updatedAt: p.UpdatedAt,
	}, nil
}

// outcomeFits: a payment that is waiting or has money held never heard the order end (a capture that
// finds an ended order goes straight on to a refund), and one that was cancelled or is being refunded
// always did.
func outcomeFits(s Status, o Outcome) bool {
	switch s {
	case StatusPending, StatusCaptured:
		return o == OutcomeNone
	case StatusCancelled, StatusRefunding, StatusRefunded:
		return o != OutcomeNone
	default:
		return true
	}
}

// validateAmount holds the amount to what kernel.NewMoney guarantees (Money's zero value and
// Multiply produce values NewMoney would have refused).
func validateAmount(m kernel.Money) error {
	if _, err := kernel.NewMoney(m.Amount(), m.Currency()); err != nil {
		return ErrInvalidAmount
	}

	return nil
}

func (x *Payment) ID() ID                       { return x.id }
func (x *Payment) OrderID() string              { return x.orderID }
func (x *Payment) CustomerID() string           { return x.customerID }
func (x *Payment) Amount() kernel.Money         { return x.amount }
func (x *Payment) Status() Status               { return x.status }
func (x *Payment) ExpiresAt() time.Time         { return x.expiresAt }
func (x *Payment) Provider() string             { return x.provider }
func (x *Payment) ProviderRef() string          { return x.providerRef }
func (x *Payment) CheckoutURL() string          { return x.checkoutURL }
func (x *Payment) CheckoutExpiresAt() time.Time { return x.checkoutExpiresAt }
func (x *Payment) Outcome() Outcome             { return x.outcome }
func (x *Payment) TerminalAt() time.Time        { return x.terminalAt }
func (x *Payment) Version() int64               { return x.version }
func (x *Payment) UpdatedAt() time.Time         { return x.updatedAt }

// CheckStart is the precondition of StartPayment (T2, column F of the table): only a PENDING
// payment whose window is still open can open a checkout.
func (x *Payment) CheckStart(now time.Time) error {
	if x.status != StatusPending {
		return ErrNotPending
	}
	if !now.Before(x.expiresAt) {
		return ErrExpired
	}

	return nil
}

// ReusableCheckout returns the checkout URL StartPayment hands out again (T2): the one opened
// earlier while its page is still valid.
func (x *Payment) ReusableCheckout(now time.Time) (string, bool) {
	if x.checkoutURL == "" || !now.Before(x.checkoutExpiresAt) {
		return "", false
	}

	return x.checkoutURL, true
}

// AttachCheckout records the hosted checkout the provider opened (T2). The status does not change.
func (x *Payment) AttachCheckout(provider, ref, url string, checkoutExpiresAt, now time.Time) error {
	if err := x.CheckStart(now); err != nil {
		return err
	}
	if provider == "" || ref == "" || url == "" || !checkoutExpiresAt.After(now) {
		return ErrInvalidCheckout
	}
	x.provider, x.providerRef, x.checkoutURL, x.checkoutExpiresAt = provider, ref, url, checkoutExpiresAt
	x.updatedAt = now

	return nil
}

// DuplicateConfirmed is OrderConfirmed reaching a payment that already exists (columns A and A'):
// the same event again is ignored; one without a total is refused and alerted.
func (x *Payment) DuplicateConfirmed(hasTotal bool) (Effect, error) {
	if !hasTotal {
		return Effect{alert: AlertMissingTotal}, ErrMissingTotal
	}

	return Effect{}, nil
}

// OrderCancelled is the order being cancelled (column B, T7 / T10 / T12 / T13).
func (x *Payment) OrderCancelled(now time.Time) Effect {
	return x.orderEnded(OutcomeCancelled, now)
}

// OrderCompleted is the order being completed (column C, T7' / T12 / T13). A payment that already
// holds the money is the normal path (the order was completed because of it) and does not change.
func (x *Payment) OrderCompleted(now time.Time) Effect {
	return x.orderEnded(OutcomeCompletedUnpaid, now)
}

func (x *Payment) orderEnded(outcome Outcome, now time.Time) Effect {
	switch x.status {
	case StatusPending:
		// T7 / T7': no one can pay an ended order. Cancel the checkout if one was opened.
		x.outcome = outcome
		x.setStatus(StatusCancelled, now)
		x.terminalAt = now

		return Effect{cancelCheckout: x.providerRef != ""}
	case StatusCaptured:
		if outcome == OutcomeCompletedUnpaid {
			return Effect{} // the order completed because of this payment
		}
		// T10: money is held for an order that was cancelled.
		x.outcome = outcome

		return x.requestRefund(now, nil)
	case StatusExpired, StatusNeedsReview:
		// T12: only remember it. A late capture of an expired payment then meets the outcome and is
		// refunded; a payment under review is not moved by the system.
		x.recordOutcome(outcome, now)

		return Effect{}
	default:
		// T13 and the refund states: the outcome is already on the payment.
		return Effect{}
	}
}

func (x *Payment) recordOutcome(outcome Outcome, now time.Time) {
	if x.outcome != OutcomeNone {
		return
	}
	x.outcome = outcome
	x.updatedAt = now
}

// Capture is money reported for this payment (columns D, D', G', G”; T3 / T4 / T6 / T6' / T8 / T9).
// got is what the provider says it collected; it must equal the payment's amount and currency to
// count. src only decides whether a repeat or mismatch on a settled payment is worth an alert: the
// expire sweep asking again is not news, a second webhook is.
func (x *Payment) Capture(src Source, got kernel.Money, now time.Time) Effect {
	match := got == x.amount
	switch x.status {
	case StatusPending, StatusExpired, StatusCancelled:
		if !match {
			// T4 / T9: not collected as a payment, an operator settles it with the provider.
			x.setStatus(StatusNeedsReview, now)

			return Effect{alert: AlertAmountMismatch}
		}
		// T3 / T6 / T8: the money is in.
		x.setStatus(StatusCaptured, now)
		eff := Effect{publish: []Event{EventCaptured}}
		if x.outcome != OutcomeNone {
			// T10: the order had already ended, the money goes back.
			return x.requestRefund(now, &eff)
		}

		return eff
	case StatusNeedsReview:
		if src == SourceWebhook {
			return Effect{alert: AlertCaptureAfterReview}
		}

		return Effect{}
	default: // CAPTURED, REFUNDING, REFUNDED: a repeat
		if !match && src == SourceWebhook {
			return Effect{alert: AlertAmountMismatch}
		}

		return Effect{}
	}
}

// requestRefund moves a CAPTURED payment to REFUNDING (T10). into is the effect to extend, or nil.
func (x *Payment) requestRefund(now time.Time, into *Effect) Effect {
	x.setStatus(StatusRefunding, now)
	if into == nil {
		return Effect{publish: []Event{EventRefundRequested}}
	}
	into.publish = append(into.publish, EventRefundRequested)

	return *into
}

// Unpaid is the expire sweep finding no money at the provider (column G, T5): a PENDING payment
// whose window has closed becomes EXPIRED. Before the window closes, and in every other status,
// it changes nothing. The caller must have asked the provider first; "could not ask" is not "no money".
func (x *Payment) Unpaid(now time.Time) Effect {
	if x.status != StatusPending || now.Before(x.expiresAt) {
		return Effect{}
	}
	x.setStatus(StatusExpired, now)
	x.terminalAt = now

	return Effect{publish: []Event{EventExpired}}
}

// RefundSucceeded is the provider confirming the refund (column H, T11). A repeat on a REFUNDED
// payment changes nothing (the provider call is idempotent on the payment id); any other status has
// no refund in progress, which is refused and leaves the payment as it was.
func (x *Payment) RefundSucceeded(now time.Time) (Effect, error) {
	switch x.status {
	case StatusRefunding:
		x.setStatus(StatusRefunded, now)

		return Effect{publish: []Event{EventRefunded}}, nil
	case StatusRefunded:
		return Effect{}, nil
	default:
		return Effect{}, ErrNotRefunding
	}
}

// ReconcileDeadline is when the sweep stops asking the provider whether money arrived on an EXPIRED or
// CANCELLED payment: terminal_at + window (docs/payment.md §3「補查有界」). The zero Time for a payment
// that is in neither state.
func (x *Payment) ReconcileDeadline(window time.Duration) time.Time {
	if (x.status != StatusExpired && x.status != StatusCancelled) || x.terminalAt.IsZero() {
		return time.Time{}
	}

	return x.terminalAt.Add(window)
}

// Reconcilable says whether the sweep should ask the provider about this payment now: it ended without
// money (EXPIRED / CANCELLED), a checkout was opened at the provider (a payment that never was cannot have
// been paid), and the window since it ended has not closed. A payment under review, refunding or refunded
// is never asked: the system does not move it, or it is already moving.
func (x *Payment) Reconcilable(now time.Time, window time.Duration) bool {
	deadline := x.ReconcileDeadline(window)

	return x.providerRef != "" && !deadline.IsZero() && now.Before(deadline)
}

func (x *Payment) setStatus(s Status, now time.Time) {
	x.status = s
	x.updatedAt = now
}
