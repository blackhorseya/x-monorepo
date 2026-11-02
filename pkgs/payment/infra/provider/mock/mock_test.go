package mock_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
)

func twd(t *testing.T, n int64) kernel.Money {
	t.Helper()
	m, err := kernel.NewMoney(n, "TWD")
	if err != nil {
		t.Fatal(err)
	}

	return m
}

// money is the same amount as the application boundary carries it.
func money(t *testing.T, n int64) dto.MoneyOutput {
	t.Helper()
	m := twd(t, n)

	return dto.MoneyOutput{Amount: m.Amount(), Currency: m.Currency()}
}

func TestCheckoutLifecycle(t *testing.T) {
	c := context.Background()
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	p := mock.New(mock.Options{CheckoutTTL: 5 * time.Minute, Now: func() time.Time { return now }})

	if p.Name() != "mock" || p.CheckoutValidity() != 5*time.Minute {
		t.Fatalf("name %q validity %s", p.Name(), p.CheckoutValidity())
	}
	co, err := p.CreateCheckout(c, dto.ProviderCheckoutInput{PaymentID: "pay-1", OrderID: "ord-1", Amount: money(t, 1000)})
	if err != nil {
		t.Fatal(err)
	}
	if co.Ref == "" || !strings.HasPrefix(co.URL, "https://") || !co.ExpiresAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("checkout = %+v", co)
	}
	if got, err := p.GetPayment(c, co.Ref); err != nil || got.Paid {
		t.Fatalf("before paying: %+v, %v", got, err)
	}

	// the customer pays a different amount: the mock reports exactly what was paid
	if _, err = p.Pay(co.Ref, twd(t, 999)); err != nil {
		t.Fatal(err)
	}
	got, err := p.GetPayment(c, co.Ref)
	if err != nil || !got.Paid || got.Amount != money(t, 999) {
		t.Fatalf("after paying: %+v, %v", got, err)
	}

	// refund: idempotent on the key
	for range 3 {
		if err = p.Refund(c, dto.ProviderRefundInput{IdempotencyKey: "pay-1", Ref: co.Ref, Amount: money(t, 999)}); err != nil {
			t.Fatal(err)
		}
	}
	if p.Refunds(co.Ref) != 1 {
		t.Fatalf("refunds = %d, want 1 (same key three times)", p.Refunds(co.Ref))
	}
	if err = p.CancelCheckout(c, co.Ref); err != nil || !p.Cancelled(co.Ref) {
		t.Fatalf("cancel: %v cancelled=%v", err, p.Cancelled(co.Ref))
	}
	if p.Opened() != 1 {
		t.Fatalf("opened = %d", p.Opened())
	}
}

func TestUnknownReferencesAreUnavailable(t *testing.T) {
	c := context.Background()
	p := mock.New(mock.Options{})
	if _, err := p.GetPayment(c, "nope"); !errors.Is(err, port.ErrProviderUnavailable) {
		t.Fatalf("GetPayment: %v", err)
	}
	if err := p.CancelCheckout(c, "nope"); !errors.Is(err, port.ErrProviderUnavailable) {
		t.Fatalf("CancelCheckout: %v", err)
	}
	if err := p.Refund(c, dto.ProviderRefundInput{IdempotencyKey: "k", Ref: "nope"}); !errors.Is(err, port.ErrProviderUnavailable) {
		t.Fatalf("Refund: %v", err)
	}
	if _, err := p.Pay("nope", twd(t, 1)); err == nil {
		t.Fatal("Pay of an unknown checkout succeeded")
	}
	// a checkout nobody paid cannot be refunded
	co, _ := p.CreateCheckout(c, dto.ProviderCheckoutInput{PaymentID: "pay-1", OrderID: "ord-1", Amount: money(t, 1)})
	if err := p.Refund(c, dto.ProviderRefundInput{IdempotencyKey: "k", Ref: co.Ref}); !errors.Is(err, port.ErrProviderUnavailable) {
		t.Fatalf("Refund of an unpaid checkout: %v", err)
	}
	if p.Cancelled("nope") || p.Refunds("nope") != 0 {
		t.Fatal("assertions on an unknown checkout must be false / zero")
	}
}

// The signature is over the raw bytes: one changed byte, or the same JSON with its keys in another order,
// does not verify (docs/payment.md §7 Webhook 1).
func TestWebhookSignatureCoversTheRawBody(t *testing.T) {
	p := mock.New(mock.Options{Secret: []byte("0123456789abcdef0123456789abcdef")})
	d := p.Notify("evt-1", mock.WebhookCaptured, "ref-1", twd(t, 1000))

	ev, err := p.VerifyWebhook(d.Body, d.Signature)
	if err != nil || ev.ID != "evt-1" || ev.Type != mock.WebhookCaptured || ev.Ref != "ref-1" || ev.Amount != money(t, 1000) {
		t.Fatalf("verify = %+v, %v", ev, err)
	}

	flipped := bytes.Replace(d.Body, []byte("1000"), []byte("1001"), 1)
	if bytes.Equal(flipped, d.Body) {
		t.Fatal("test bug: nothing was changed")
	}
	if _, err = p.VerifyWebhook(flipped, d.Signature); !errors.Is(err, port.ErrInvalidSignature) {
		t.Fatalf("changed byte: %v, want ErrInvalidSignature", err)
	}
	reordered := []byte(`{"type":"payment.captured","id":"evt-1","ref":"ref-1","amount":1000,"currency":"TWD"}`)
	if bytes.Equal(reordered, d.Body) {
		t.Fatal("test bug: the reordering equals the original")
	}
	if _, err = p.VerifyWebhook(reordered, d.Signature); !errors.Is(err, port.ErrInvalidSignature) {
		t.Fatalf("reordered keys: %v, want ErrInvalidSignature", err)
	}
	if _, err = p.VerifyWebhook(d.Body, ""); !errors.Is(err, port.ErrInvalidSignature) {
		t.Fatalf("no signature: %v", err)
	}
	// another secret: not verifiable
	other := mock.New(mock.Options{Secret: []byte("another secret another secret 00")})
	if _, err = other.VerifyWebhook(d.Body, d.Signature); !errors.Is(err, port.ErrInvalidSignature) {
		t.Fatalf("other secret: %v", err)
	}
}

func TestRedeliveryIsByteIdentical(t *testing.T) {
	p := mock.New(mock.Options{})
	co, _ := p.CreateCheckout(context.Background(), dto.ProviderCheckoutInput{PaymentID: "pay-1", OrderID: "ord-1", Amount: money(t, 5)})
	a, err := p.Pay(co.Ref, twd(t, 5))
	if err != nil {
		t.Fatal(err)
	}
	b := p.Notify("evt_"+co.Ref+"_paid", mock.WebhookCaptured, co.Ref, twd(t, 5))
	if !bytes.Equal(a.Body, b.Body) || a.Signature != b.Signature {
		t.Fatal("the same event, delivered again, must be the same bytes and the same signature (redelivery)")
	}
}

func TestFaultAndLatency(t *testing.T) {
	c := context.Background()
	var seen []string
	p := mock.New(mock.Options{Fault: func(op string) error {
		seen = append(seen, op)
		if op == mock.OpRefund {
			return errors.New("raw provider text that must not escape")
		}

		return nil
	}})
	co, err := p.CreateCheckout(c, dto.ProviderCheckoutInput{PaymentID: "p", OrderID: "o", Amount: money(t, 1)})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = p.Pay(co.Ref, twd(t, 1))
	err = p.Refund(c, dto.ProviderRefundInput{IdempotencyKey: "p", Ref: co.Ref})
	if !errors.Is(err, port.ErrProviderUnavailable) || strings.Contains(err.Error(), "raw provider text") {
		t.Fatalf("a faulting refund = %v, want only the fixed sentinel", err)
	}
	if len(seen) != 2 || seen[0] != mock.OpCreateCheckout || seen[1] != mock.OpRefund {
		t.Fatalf("faults asked for %v", seen)
	}

	slow := mock.New(mock.Options{Latency: time.Hour})
	cc, cancel := context.WithTimeout(c, 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err = slow.CreateCheckout(cc, dto.ProviderCheckoutInput{}); !errors.Is(err, port.ErrProviderUnavailable) {
		t.Fatalf("slow provider with a short deadline: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the call did not honour the caller's deadline")
	}
	short := mock.New(mock.Options{Latency: time.Millisecond})
	if _, err = short.CreateCheckout(c, dto.ProviderCheckoutInput{Amount: money(t, 1)}); err != nil {
		t.Fatalf("a little latency must still succeed: %v", err)
	}
}
