package payment

import (
	"context"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

// WebhookCaptured is the one provider event type that moves a payment (docs/payment.md §7); any
// other verified type is stored, then ignored.
const WebhookCaptured = "payment.captured"

// WebhookEvent is a provider notification whose signature was verified, as it is kept in
// payment.webhook_events: the key (provider, event id) that deduplicates redeliveries, what the
// provider says happened, and the raw body as received. It is NOT a payment event: applying it to a
// payment is a separate step (`consume`), so a crash between the two loses nothing.
type WebhookEvent struct {
	provider    string
	eventID     string
	eventType   string
	ref         string
	amount      kernel.Money // zero = the notification carries no amount
	body        []byte
	receivedAt  time.Time
	processedAt time.Time
}

// NewWebhookEvent validates a verified notification. amount is the zero Money for an event without one.
func NewWebhookEvent(provider, eventID, eventType, ref string, amount kernel.Money, body []byte, at time.Time) (*WebhookEvent, error) {
	switch {
	case provider == "" || len(provider) > 64:
		return nil, ErrInvalidWebhook
	case eventID == "" || len(eventID) > 128:
		return nil, ErrInvalidWebhook
	case eventType == "" || len(eventType) > 128 || len(ref) > 128:
		return nil, ErrInvalidWebhook
	case len(body) == 0 || len(body) > MaxWebhookBody:
		return nil, ErrInvalidWebhook
	}
	if amount != (kernel.Money{}) {
		if err := validateAmount(amount); err != nil {
			return nil, ErrInvalidWebhook
		}
	}

	return &WebhookEvent{provider: provider, eventID: eventID, eventType: eventType, ref: ref, amount: amount, body: append([]byte(nil), body...), receivedAt: at}, nil
}

// MaxWebhookBody bounds a notification body (bytes). The route enforces it before reading, the schema again.
const MaxWebhookBody = 1 << 20

// ReconstituteWebhookParams carry a stored notification back.
type ReconstituteWebhookParams struct {
	Provider, EventID, EventType, Ref string
	Amount                            kernel.Money
	Body                              []byte
	ReceivedAt, ProcessedAt           time.Time
}

// ReconstituteWebhook rebuilds a stored notification. Only repositories should call it.
func ReconstituteWebhook(p ReconstituteWebhookParams) (*WebhookEvent, error) {
	w, err := NewWebhookEvent(p.Provider, p.EventID, p.EventType, p.Ref, p.Amount, p.Body, p.ReceivedAt)
	if err != nil {
		return nil, err
	}
	w.processedAt = p.ProcessedAt

	return w, nil
}

func (x *WebhookEvent) Provider() string       { return x.provider }
func (x *WebhookEvent) EventID() string        { return x.eventID }
func (x *WebhookEvent) EventType() string      { return x.eventType }
func (x *WebhookEvent) Ref() string            { return x.ref }
func (x *WebhookEvent) Amount() kernel.Money   { return x.amount }
func (x *WebhookEvent) Body() []byte           { return append([]byte(nil), x.body...) }
func (x *WebhookEvent) ReceivedAt() time.Time  { return x.receivedAt }
func (x *WebhookEvent) ProcessedAt() time.Time { return x.processedAt }

// IsCapture says whether the notification reports money collected.
func (x *WebhookEvent) IsCapture() bool { return x.eventType == WebhookCaptured }

// Processed says whether the notification has been applied (or deliberately ignored).
func (x *WebhookEvent) Processed() bool { return !x.processedAt.IsZero() }

// MarkProcessed records that the notification has been dealt with, once: a second call keeps the first time.
func (x *WebhookEvent) MarkProcessed(at time.Time) {
	if x.Processed() {
		return
	}
	x.processedAt = at
}

// WebhookRepository keeps the verified notifications.
type WebhookRepository interface {
	// Insert stores w unless a notification with the same (provider, event id) is already there:
	// inserted is false for a redelivery, and nothing changes.
	Insert(c context.Context, w *WebhookEvent) (inserted bool, err error)
	// Find returns ErrNotFound when there is no such notification.
	Find(c context.Context, provider, eventID string) (*WebhookEvent, error)
	// MarkProcessed sets processed_at on a notification that has none; an already processed one is untouched.
	MarkProcessed(c context.Context, provider, eventID string, at time.Time) error
	// DeleteProcessedBefore deletes up to limit PROCESSED notifications processed before cutoff, oldest
	// first, and returns how many. Unprocessed ones are never touched, whatever their age.
	DeleteProcessedBefore(c context.Context, cutoff time.Time, limit int) (int, error)
}
