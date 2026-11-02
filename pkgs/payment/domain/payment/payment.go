// Package payment is the Payment aggregate of the payment bounded context (docs/payment.md §3).
//
// SPEC STAGE: this file is the API the spec compiles against with no behaviour behind it, so the
// tests fail on their assertions. The implementation replaces it in the next commit.
package payment

import (
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

// ID identifies a Payment.
type ID string

func (x ID) String() string { return string(x) }

// Payment is the aggregate root.
type Payment struct {
	id                ID
	orderID           string
	customerID        string
	amount            kernel.Money
	status            Status
	expiresAt         time.Time
	provider          string
	providerRef       string
	checkoutURL       string
	checkoutExpiresAt time.Time
	outcome           Outcome
	terminalAt        time.Time
	version           int64
	updatedAt         time.Time
}

// NewParams is what OrderConfirmed carries (T1).
type NewParams struct {
	ID         ID
	OrderID    string
	CustomerID string
	Total      kernel.Money
	ExpiresAt  time.Time
}

// New creates a PENDING payment. Spec stage: nothing is validated.
func New(p NewParams, now time.Time) (*Payment, error) {
	return &Payment{id: p.ID, orderID: p.OrderID, customerID: p.CustomerID, amount: p.Total, status: StatusPending, expiresAt: p.ExpiresAt, outcome: OutcomeNone, updatedAt: now}, nil
}

// NewForEndedOrder creates a CANCELLED payment (T14). Spec stage: nothing is validated, it stays PENDING.
func NewForEndedOrder(p NewParams, _ Outcome, now time.Time) (*Payment, error) {
	return New(p, now)
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

// Reconstitute rebuilds a Payment from persistence. Spec stage: nothing is validated.
func Reconstitute(p ReconstituteParams) (*Payment, error) {
	return &Payment{
		id: p.ID, orderID: p.OrderID, customerID: p.CustomerID, amount: p.Amount, status: p.Status,
		expiresAt: p.ExpiresAt, provider: p.Provider, providerRef: p.ProviderRef, checkoutURL: p.CheckoutURL,
		checkoutExpiresAt: p.CheckoutExpiresAt, outcome: p.Outcome, terminalAt: p.TerminalAt,
		version: p.Version, updatedAt: p.UpdatedAt,
	}, nil
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

// The transitions of docs/payment.md §3. Spec stage: none of them does anything.

func (x *Payment) CheckStart(time.Time) error { return nil }

func (x *Payment) ReusableCheckout(time.Time) (string, bool) { return "", false }

func (x *Payment) AttachCheckout(_, _, _ string, _, _ time.Time) error { return nil }

func (x *Payment) DuplicateConfirmed(bool) (Effect, error) { return Effect{}, nil }

func (x *Payment) OrderCancelled(time.Time) Effect { return Effect{} }

func (x *Payment) OrderCompleted(time.Time) Effect { return Effect{} }

func (x *Payment) Capture(Source, kernel.Money, time.Time) Effect { return Effect{} }

func (x *Payment) Unpaid(time.Time) Effect { return Effect{} }

func (x *Payment) RefundSucceeded(time.Time) (Effect, error) { return Effect{}, nil }
