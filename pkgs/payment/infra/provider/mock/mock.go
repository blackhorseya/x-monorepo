// Package mock is the first version's only PaymentProvider (docs/payment.md §7): an in-memory
// gateway that opens "hosted checkouts" nobody can really pay, so tests and a local stack can walk
// the whole flow. It signs the webhooks it makes the way a real provider would (HMAC-SHA256 over the
// raw body) and can be slow or broken on demand.
//
// It is a tool for tests and development: a deployment that charges real money needs a real provider.
package mock

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/idgen"
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

// Options configure a Provider. The zero value is a working mock whose checkouts may last up to 30 minutes.
type Options struct {
	// CheckoutTTL is the longest an opened page can stay valid (port.PaymentProvider.CheckoutValidity); a
	// checkout is opened to end when the caller asks, up to this. Default 30 minutes.
	CheckoutTTL time.Duration
	// RedeliveryWindow is how long the mock says it may resend a notification; default 72 hours.
	RedeliveryWindow time.Duration
	// Secret signs the webhooks; empty = a random one for this process.
	Secret []byte
	// Latency delays every call (it honours the caller's context), to rehearse a slow provider.
	Latency time.Duration
	// Fault, when set, is asked before every call with its operation name; a non-nil answer fails the
	// call. The call still ends as port.ErrProviderUnavailable for the caller (the answer is the
	// "provider's own error text" that must never get out). Tests use it to inject failures.
	Fault func(op string) error
	// Now is the clock; default time.Now().UTC().
	Now func() time.Time
}

// Provider is the mock gateway. Safe for concurrent use.
type Provider struct {
	opts Options

	mu        sync.Mutex
	refs      []string // checkout refs in the order they were opened (for assertions)
	checkouts map[string]*checkout
}

type checkout struct {
	paymentID string
	amount    dto.MoneyOutput
	url       string
	expiresAt time.Time
	paid      bool
	paidWith  dto.MoneyOutput
	cancelled bool
	refunded  map[string]struct{} // idempotency keys already honoured
}

// New creates a mock provider.
func New(opts Options) *Provider {
	if opts.CheckoutTTL <= 0 {
		opts.CheckoutTTL = 30 * time.Minute
	}
	if opts.RedeliveryWindow <= 0 {
		opts.RedeliveryWindow = 72 * time.Hour
	}
	if len(opts.Secret) == 0 {
		opts.Secret = make([]byte, 32)
		_, _ = rand.Read(opts.Secret) // crypto/rand does not fail on supported platforms
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}

	return &Provider{opts: opts, checkouts: map[string]*checkout{}}
}

func (x *Provider) Name() string { return Name }

func (x *Provider) CheckoutValidity() time.Duration { return x.opts.CheckoutTTL }

func (x *Provider) WebhookRedeliveryWindow() time.Duration { return x.opts.RedeliveryWindow }

// enter is what every call starts with: the injected latency, then the injected fault.
func (x *Provider) enter(c context.Context, op string) error {
	if x.opts.Latency > 0 {
		select {
		case <-time.After(x.opts.Latency):
		case <-c.Done():
			return port.ErrProviderUnavailable
		}
	}
	if x.opts.Fault != nil && x.opts.Fault(op) != nil {
		return port.ErrProviderUnavailable
	}

	return nil
}

func (x *Provider) CreateCheckout(c context.Context, req dto.ProviderCheckoutInput) (dto.ProviderCheckoutOutput, error) {
	if err := x.enter(c, OpCreateCheckout); err != nil {
		return dto.ProviderCheckoutOutput{}, err
	}

	// the page ends when the caller says (the payment's window), if the provider can keep one that long
	now := x.opts.Now()
	ends := now.Add(x.opts.CheckoutTTL)
	if !req.ExpiresAt.IsZero() {
		if req.ExpiresAt.After(ends) {
			return dto.ProviderCheckoutOutput{}, port.ErrProviderUnavailable
		}
		ends = req.ExpiresAt
	}

	// The ref is the key the payment table and the webhook log keep across every instance and restart
	// that shares the secret, so it comes from the system's id generator (UUID v7), never from a
	// counter that starts again in each process.
	ref, err := idgen.New()
	if err != nil {
		return dto.ProviderCheckoutOutput{}, fmt.Errorf("mock: mint a checkout ref: %w", port.ErrProviderUnavailable)
	}

	x.mu.Lock()
	defer x.mu.Unlock()
	co := &checkout{
		paymentID: req.PaymentID, amount: req.Amount, url: "https://mock-pay.invalid/checkout/" + ref,
		expiresAt: ends, refunded: map[string]struct{}{},
	}
	x.checkouts[ref] = co
	x.refs = append(x.refs, ref)

	return dto.ProviderCheckoutOutput{Ref: ref, URL: co.url, ExpiresAt: co.expiresAt}, nil
}

func (x *Provider) GetPayment(c context.Context, ref string) (dto.ProviderPaymentOutput, error) {
	if err := x.enter(c, OpGetPayment); err != nil {
		return dto.ProviderPaymentOutput{}, err
	}

	x.mu.Lock()
	defer x.mu.Unlock()
	co, ok := x.checkouts[ref]
	if !ok {
		return dto.ProviderPaymentOutput{}, port.ErrProviderUnavailable
	}
	if !co.paid {
		return dto.ProviderPaymentOutput{}, nil
	}

	return dto.ProviderPaymentOutput{Paid: true, Amount: co.paidWith}, nil
}

func (x *Provider) CancelCheckout(c context.Context, ref string) error {
	if err := x.enter(c, OpCancelCheckout); err != nil {
		return err
	}

	x.mu.Lock()
	defer x.mu.Unlock()
	co, ok := x.checkouts[ref]
	if !ok {
		return port.ErrProviderUnavailable
	}
	co.cancelled = true

	return nil
}

func (x *Provider) Refund(c context.Context, req dto.ProviderRefundInput) error {
	if err := x.enter(c, OpRefund); err != nil {
		return err
	}

	x.mu.Lock()
	defer x.mu.Unlock()
	co, ok := x.checkouts[req.Ref]
	if !ok || !co.paid {
		return port.ErrProviderUnavailable
	}
	co.refunded[req.IdempotencyKey] = struct{}{} // asking again with the same key refunds nothing twice

	return nil
}

// event is the JSON body of a mock webhook.
type event struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Ref      string `json:"ref"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

func (x *Provider) sign(body []byte) string {
	mac := hmac.New(sha256.New, x.opts.Secret)
	mac.Write(body)

	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyWebhook checks the signature over the RAW body (not a re-encoding of it) and decodes it.
func (x *Provider) VerifyWebhook(body []byte, signature string) (dto.ProviderWebhookOutput, error) {
	if !hmac.Equal([]byte(x.sign(body)), []byte(signature)) {
		return dto.ProviderWebhookOutput{}, port.ErrInvalidSignature
	}
	var ev event
	if err := json.Unmarshal(body, &ev); err != nil {
		return dto.ProviderWebhookOutput{}, errors.New("webhook body is not an event")
	}
	if _, err := kernel.NewMoney(ev.Amount, ev.Currency); err != nil {
		return dto.ProviderWebhookOutput{}, errors.New("webhook body is not an event")
	}

	return dto.ProviderWebhookOutput{ID: ev.ID, Type: ev.Type, Ref: ev.Ref, Amount: dto.MoneyOutput{Amount: ev.Amount, Currency: ev.Currency}}, nil
}

// Delivery is one webhook the mock would POST: the raw body and its signature header value.
type Delivery struct {
	Body      []byte
	Signature string
}

// Pay plays the customer paying on the hosted page for amount (which need not be the checkout's:
// a mismatch is a case worth rehearsing) and returns the signed "payment.captured" webhook. The
// same Delivery may be handed over any number of times, in any order, to rehearse redelivery. It
// makes no call to the payment service: delivering it is the test's job.
func (x *Provider) Pay(ref string, amount kernel.Money) (Delivery, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	co, ok := x.checkouts[ref]
	if !ok {
		return Delivery{}, fmt.Errorf("mock: no checkout %q", ref)
	}
	co.paid, co.paidWith = true, dto.MoneyOutput{Amount: amount.Amount(), Currency: amount.Currency()}

	return x.delivery(fmt.Sprintf("evt_%s_paid", ref), WebhookCaptured, ref, amount), nil
}

// Notify makes a signed webhook of any type for a checkout, without moving it.
func (x *Provider) Notify(id, typ, ref string, amount kernel.Money) Delivery {
	x.mu.Lock()
	defer x.mu.Unlock()

	return x.delivery(id, typ, ref, amount)
}

func (x *Provider) delivery(id, typ, ref string, amount kernel.Money) Delivery {
	body, _ := json.Marshal(event{ID: id, Type: typ, Ref: ref, Amount: amount.Amount(), Currency: amount.Currency()})

	return Delivery{Body: body, Signature: x.sign(body)}
}

// Cancelled reports whether CancelCheckout reached the checkout (for assertions).
func (x *Provider) Cancelled(ref string) bool {
	x.mu.Lock()
	defer x.mu.Unlock()

	co, ok := x.checkouts[ref]

	return ok && co.cancelled
}

// Refunds counts the distinct idempotency keys the checkout was refunded with (for assertions).
func (x *Provider) Refunds(ref string) int {
	x.mu.Lock()
	defer x.mu.Unlock()

	co, ok := x.checkouts[ref]
	if !ok {
		return 0
	}

	return len(co.refunded)
}

// Refs lists the refs of the checkouts ever created, oldest first (for assertions).
func (x *Provider) Refs() []string {
	x.mu.Lock()
	defer x.mu.Unlock()

	return append([]string(nil), x.refs...)
}

// Opened counts the checkouts ever created (for assertions).
func (x *Provider) Opened() int {
	x.mu.Lock()
	defer x.mu.Unlock()

	return len(x.checkouts)
}
