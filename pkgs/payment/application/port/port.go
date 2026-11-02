// Package port declares what the payment use cases need from the outside world. Implementations
// live in payment/infra.
package port

import (
	"context"
	"errors"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
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

// ErrProviderUnavailable is a failure to get a usable answer from the payment provider (as opposed
// to the provider answering "no"). PaymentProvider implementations return it, use cases pass it on
// WITHOUT the provider's own error: a provider's error text is untrusted and may reflect a credential
// (#347). The rpc edge maps it to UNAVAILABLE with a fixed message. It is deliberately not a
// kernel.Error: nothing about the payment is wrong.
var ErrProviderUnavailable = errors.New("payment provider unavailable")

// ErrInvalidSignature is a webhook whose signature does not verify against its raw body.
var ErrInvalidSignature = errors.New("webhook signature invalid")

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
