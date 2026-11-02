// Package mock is the first version's only PaymentProvider (docs/payment.md §7).
// SPEC STAGE: the API without the gateway behind it; every call answers with nothing.
package mock

import (
	"context"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
)

// Name is what the mock stores on a payment as its provider.
const Name = "mock"

// WebhookCaptured is the one event type that moves a payment (docs/payment.md §7).
const WebhookCaptured = "payment.captured"

// Operation names for Options.Fault.
const (
	OpCreateCheckout = "create_checkout"
	OpGetPayment     = "get_payment"
	OpCancelCheckout = "cancel_checkout"
	OpRefund         = "refund"
)

var _ port.PaymentProvider = (*Provider)(nil)

// Options configure a Provider.
type Options struct {
	CheckoutTTL time.Duration
	Secret      []byte
	Latency     time.Duration
	Fault       func(op string) error
	Now         func() time.Time
}

// Provider is the mock gateway.
type Provider struct{ opts Options }

// New creates a mock provider.
func New(opts Options) *Provider { return &Provider{opts: opts} }

func (x *Provider) Name() string { return Name }

func (x *Provider) CheckoutValidity() time.Duration { return x.opts.CheckoutTTL }

func (x *Provider) CreateCheckout(context.Context, dto.ProviderCheckoutInput) (dto.ProviderCheckoutOutput, error) {
	return dto.ProviderCheckoutOutput{}, nil
}

func (x *Provider) GetPayment(context.Context, string) (dto.ProviderPaymentOutput, error) {
	return dto.ProviderPaymentOutput{}, nil
}

func (x *Provider) CancelCheckout(context.Context, string) error { return nil }

func (x *Provider) Refund(context.Context, dto.ProviderRefundInput) error { return nil }

func (x *Provider) sign([]byte) string { return "" }

func (x *Provider) VerifyWebhook([]byte, string) (dto.ProviderWebhookOutput, error) {
	return dto.ProviderWebhookOutput{}, nil
}

// Delivery is one webhook the mock would POST.
type Delivery struct {
	Body      []byte
	Signature string
}

func (x *Provider) Pay(string, kernel.Money) (Delivery, error) { return Delivery{}, nil }

func (x *Provider) Notify(string, string, string, kernel.Money) Delivery { return Delivery{} }

func (x *Provider) Cancelled(string) bool { return false }

func (x *Provider) Refunds(string) int { return 0 }

func (x *Provider) Opened() int { return 0 }
