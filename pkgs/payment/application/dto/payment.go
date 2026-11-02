// Package dto is the hand-written application boundary of the payment use cases: only what they
// need, not a copy of the wire format.
package dto

import "time"

// MoneyOutput is an amount in minor units and its ISO 4217 currency.
type MoneyOutput struct {
	Amount   int64
	Currency string
}

// PaymentOutput is one payment as use cases return it. The provider's reference is not part of it:
// a customer has no use for it.
type PaymentOutput struct {
	ID                string
	OrderID           string
	CustomerID        string
	Amount            MoneyOutput
	Status            string
	ExpiresAt         time.Time
	CheckoutURL       string
	CheckoutExpiresAt time.Time
	UpdateTime        time.Time
}

// StartPaymentInput is the caller's claim about the end user (see PaymentService): CustomerID is who
// is paying, Admin that the caller holds the admin role.
type StartPaymentInput struct {
	OrderID    string
	CustomerID string
	Admin      bool
}

// GetPaymentInput: an admin or the order's own customer may read it.
type GetPaymentInput struct {
	OrderID    string
	CustomerID string
	Admin      bool
}

// OrderConfirmedInput is OrderConfirmed as the payment consumer reads it. Total is nil when the
// event carried none (published before the field existed): that is refused, never collected as 0.
type OrderConfirmedInput struct {
	OrderID    string
	CustomerID string
	Total      *MoneyOutput
}

// OrderEndedInput is OrderCancelled or OrderCompleted (two use cases, one shape).
type OrderEndedInput struct {
	OrderID string
}

// OrderEndedOutput tells the decorator that runs after the transaction committed whether a
// provider checkout is to be cancelled (best effort) and which one.
type OrderEndedOutput struct {
	CancelCheckoutRef string
}

// RefundInput is PaymentRefundRequested: the payment whose refund is to be made.
type RefundInput struct {
	PaymentID string
}

// Ack is the answer of a use case that has nothing to say but "done".
type Ack struct{}
