// Package port declares what the payment use cases need from the outside world. Implementations
// live in payment/infra.
package port

import (
	"context"
	"errors"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
)

// Clock abstracts time for deterministic tests.
type Clock interface {
	Now() time.Time
}

// Transactor runs fn atomically: every repository call inside fn commits together or not at all.
// Use cases that call a provider hold it (docs/payment.md §1 例外表) and wrap only the write in it;
// the provider is never called inside fn. The memory implementation just calls fn.
type Transactor interface {
	Do(c context.Context, fn func(c context.Context) error) error
}

// Inbox is the consumer inbox (usecasex.Idempotent): Record reports whether the message id is new.
// provider.go wraps every use case a consumer drives with it, inside the transaction, so a
// redelivery is dropped before any aggregate is loaded. nil Inbox (no database) makes it a pass-through.
type Inbox interface {
	Record(c context.Context, id string) (first bool, err error)
}

// IDGenerator creates the ids of new payments (UUID v7, #365).
type IDGenerator interface {
	NewID() (string, error)
}

// EventPublisher hands the events a transition raised to the outbox. Called inside Transactor.Do
// (or the Transactional decorator), so the rows share the state change's transaction. at is the
// domain time of the transition (port.Clock), not wall time.
type EventPublisher interface {
	// Publish maps each of events, as raised on p, to its payment.v1 message.
	Publish(c context.Context, p *payment.Payment, events []payment.Event, at time.Time) error
	// PublishWebhookReceived queues the processing of a stored provider notification.
	PublishWebhookReceived(c context.Context, provider, providerEventID string, at time.Time) error
}

// Alerts tells operators that something needs a human (docs/payment.md "告警"): money that does not
// match a payment, a notification nobody can place, an event that must not be collected. The
// implementation logs a fixed category and counts it; it never carries provider or customer text.
type Alerts interface {
	Raise(c context.Context, a payment.Alert, orderID string)
}

// ErrProviderUnavailable is a failure to get a usable answer from the payment provider (as opposed
// to the provider answering "no"). PaymentProvider implementations return it, use cases pass it on
// WITHOUT the provider's own error: a provider's error text is untrusted and may reflect a credential
// (#347). The rpc edge maps it to UNAVAILABLE with a fixed message. It is deliberately not a
// kernel.Error: nothing about the payment is wrong.
var ErrProviderUnavailable = errors.New("payment provider unavailable")

// ErrInvalidSignature is a webhook whose signature does not verify against its raw body.
var ErrInvalidSignature = errors.New("webhook signature invalid")

// ErrMalformedWebhook is a notification whose signature verified but whose content is not an event a
// payment can be told about. Like every provider-facing error it carries none of the provider's words.
var ErrMalformedWebhook = errors.New("webhook malformed")

// PaymentProvider is the payment gateway behind a hosted checkout. The first version has only a mock.
// Every method returns ErrProviderUnavailable (never the provider's own error text) when the provider
// cannot give a usable answer.
type PaymentProvider interface {
	// Name is the provider's name as stored on the payment ("mock").
	Name() string
	// CheckoutValidity is the longest a checkout page it opens stays valid. docs/payment.md §3: the
	// reconcile window must exceed it.
	CheckoutValidity() time.Duration
	// WebhookRedeliveryWindow is how long after a notification the provider may send it again. The
	// retention of processed payment.webhook_events rows must outlast it, or a redelivery would be
	// taken for a new event.
	WebhookRedeliveryWindow() time.Duration
	CreateCheckout(c context.Context, in dto.ProviderCheckoutInput) (dto.ProviderCheckoutOutput, error)
	GetPayment(c context.Context, ref string) (dto.ProviderPaymentOutput, error)
	CancelCheckout(c context.Context, ref string) error
	Refund(c context.Context, in dto.ProviderRefundInput) error
	// VerifyWebhook checks the signature over the RAW body and decodes it. ErrInvalidSignature when
	// the signature does not match.
	VerifyWebhook(body []byte, signature string) (dto.ProviderWebhookOutput, error)
}
