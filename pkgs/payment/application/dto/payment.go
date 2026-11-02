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

// WebhookInput is one POST of the provider's webhook as the route read it: the RAW body (the signature
// is over these bytes, not over a re-encoding) and the signature header's value.
type WebhookInput struct {
	Body      []byte
	Signature string
}

// WebhookReceivedInput names a stored notification (payment.v1.WebhookReceived): the storage key only.
type WebhookReceivedInput struct {
	Provider string
	EventID  string
}

// ExpireOutput is what one run of the expire sweep did, for the log: counts only.
type ExpireOutput struct {
	// Expired: PENDING payments past their window with no money at the provider (T5).
	Expired int
	// Captured: payments the sweep found money for that the webhook never reported (T6, T8).
	Captured int
	// NeedsReview: payments the sweep found money for that does not match (T6', T9).
	NeedsReview int
	// QueryFailures: provider queries that did not answer. The payment did not move.
	QueryFailures int
	// Conflicts: payments someone else changed meanwhile; they come round again next run.
	Conflicts int
	// Failures: payments whose write failed for another reason. The run still finishes the rest, then fails.
	Failures int
	// RetentionDeleted: processed notifications deleted by retention.
	RetentionDeleted int
	// RetentionFailed: the retention delete failed; the run's transitions still happened.
	RetentionFailed bool
}

// SweepConfig is the expire sweep's settings, mapped from config.Payment by the composition root.
type SweepConfig struct {
	// ReconcileWindow: how long after a payment became EXPIRED / CANCELLED the provider is still asked
	// whether money arrived (docs/payment.md §3「補查有界」).
	ReconcileWindow time.Duration
	// Interval is how often the sweep runs; a reconcile query is the payment's last when the next run
	// would fall after its window closed.
	Interval time.Duration
	// Batch bounds the payments looked at per list and the notifications deleted per run.
	Batch int
	// WebhookRetention: processed notifications older than this are deleted; 0 keeps them for ever.
	WebhookRetention time.Duration
}
