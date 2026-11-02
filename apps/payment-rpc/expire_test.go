package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	paymentbc "github.com/blackhorseya/x-monorepo/pkgs/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

// sweep runs `payment-rpc expire` once the way main builds it (InitializeExpire: the real use case, the
// real repositories, the outbox) behind provider (nil = the env's mock).
func (e *consumeEnv) sweep(provider port.PaymentProvider) error {
	e.t.Helper()
	if provider == nil {
		provider = e.prov
	}
	expire, cleanup, err := InitializeExpire(&config.AppConfig{
		Log:      config.Log{Level: "error", Format: "text"},
		Postgres: e.pg,
		Payment: config.Payment{
			Provider: "mock", Window: 30 * time.Minute, ReconcileWindow: 24 * time.Hour, MockCheckoutTTL: 30 * time.Minute,
			WebhookRetention: 720 * time.Hour, ExpireInterval: 15 * time.Minute, ExpireBatch: 100,
		},
	}, paymentbc.Fixed(provider))
	if err != nil {
		e.t.Fatal(err)
	}
	defer cleanup()

	return expire.Run(context.Background())
}

// overdue moves the window of every PENDING payment into the past, as time passing would.
func (e *consumeEnv) overdue() {
	e.t.Helper()
	if err := e.db.Exec(`UPDATE payment.payments SET expires_at = now() - interval '5 minutes' WHERE status = 'PENDING'`).Error; err != nil {
		e.t.Fatal(err)
	}
}

// failingQuery is a provider whose GetPayment fails while down is set and counts every attempt.
type failingQuery struct {
	*mock.Provider
	down  atomic.Bool
	calls atomic.Int64
}

func (f *failingQuery) GetPayment(c context.Context, ref string) (dto.ProviderPaymentOutput, error) {
	f.calls.Add(1)
	if f.down.Load() {
		return dto.ProviderPaymentOutput{}, errors.New("gateway unreachable")
	}

	return f.Provider.GetPayment(c, ref)
}

// T5 over Postgres: a payment past its window with no money at the provider is EXPIRED and PaymentExpired is
// queued exactly once, however often the sweep runs.
func TestExpire_ExpiresADuePaymentOnce(t *testing.T) {
	e := newConsumeEnv(t)
	e.pendingWithCheckout()
	e.overdue()

	for range 2 {
		if err := e.sweep(nil); err != nil {
			t.Fatal(err)
		}
	}
	p := e.mustPayment("ord-1")
	if p.Status() != payment.StatusExpired || p.TerminalAt().IsZero() {
		t.Fatalf("status %s terminal %v", p.Status(), p.TerminalAt())
	}
	if n := e.count(`SELECT count(*) FROM payment.events_v1_paymentexpired`); n != 1 {
		t.Fatalf("%d PaymentExpired events after two sweeps, want 1", n)
	}
}

func TestExpire_LeavesAPaymentInsideItsWindow(t *testing.T) {
	e := newConsumeEnv(t)
	e.pendingWithCheckout()
	if err := e.sweep(nil); err != nil {
		t.Fatal(err)
	}
	if got := e.mustPayment("ord-1").Status(); got != payment.StatusPending {
		t.Fatalf("status = %s", got)
	}
	if n := e.count(`SELECT count(*) FROM payment.events_v1_paymentexpired`); n != 0 {
		t.Fatalf("%d PaymentExpired events for a payment inside its window", n)
	}
}

// T6: the webhook never came; the sweep finds the money and captures instead of expiring.
func TestExpire_CapturesMoneyTheWebhookNeverReported(t *testing.T) {
	e := newConsumeEnv(t)
	p := e.pendingWithCheckout()
	if _, err := e.prov.Pay(p.ProviderRef(), p.Amount()); err != nil {
		t.Fatal(err)
	}
	e.overdue()
	if err := e.sweep(nil); err != nil {
		t.Fatal(err)
	}
	if got := e.mustPayment("ord-1").Status(); got != payment.StatusCaptured {
		t.Fatalf("status = %s", got)
	}
	if e.count(`SELECT count(*) FROM payment.events_v1_paymentcaptured`) != 1 || e.count(`SELECT count(*) FROM payment.events_v1_paymentexpired`) != 0 {
		t.Fatal("a paid payment must publish PaymentCaptured and never PaymentExpired")
	}
}

// §3 failure table: a provider that cannot be asked leaves the payment PENDING (never EXPIRED on a failed
// query), the sweep still succeeds, and the next run converges once the provider answers.
func TestExpire_AProviderThatAlwaysFailsMovesNothingAndConvergesAfterRecovery(t *testing.T) {
	e := newConsumeEnv(t)
	e.pendingWithCheckout()
	e.overdue()
	prov := &failingQuery{Provider: e.prov}
	prov.down.Store(true)

	for range 2 {
		if err := e.sweep(prov); err != nil {
			t.Fatalf("a failed query failed the run: %v", err)
		}
	}
	if prov.calls.Load() != 2 {
		t.Fatalf("positive control: %d queries, want 2", prov.calls.Load())
	}
	if got := e.mustPayment("ord-1").Status(); got != payment.StatusPending {
		t.Fatalf("status = %s while the provider fails, want PENDING", got)
	}
	if n := e.count(`SELECT count(*) FROM payment.events_v1_paymentexpired`); n != 0 {
		t.Fatalf("%d PaymentExpired events on a failed query", n)
	}

	prov.down.Store(false)
	if err := e.sweep(prov); err != nil {
		t.Fatal(err)
	}
	if got := e.mustPayment("ord-1").Status(); got != payment.StatusExpired {
		t.Fatalf("status = %s after recovery", got)
	}
}

// Retention over Postgres, from the real entry: processed notifications older than payment.webhook_retention are
// deleted, and the unprocessed one, the recent one and the one of a payment under review are not.
func TestExpire_RetentionDeletesOnlyOldProcessedNotifications(t *testing.T) {
	e := newConsumeEnv(t)
	review := e.pendingWithCheckout()
	short, _ := kernel.NewMoney(1, "TWD")
	review.Capture(payment.SourceWebhook, short, time.Now()) // does not match: NEEDS_REVIEW
	if err := e.repo().Save(context.Background(), review); err != nil {
		t.Fatal(err)
	}
	if got := e.mustPayment("ord-1").Status(); got != payment.StatusNeedsReview {
		t.Fatalf("fixture status = %s", got)
	}

	add := func(id, ref string, processedAgo time.Duration) {
		t.Helper()
		q := `INSERT INTO payment.webhook_events (provider, provider_event_id, event_type, provider_ref, body, received_at, processed_at)
		      VALUES ('mock', ?, 'payment.captured', ?, '\x7b7d', now() - interval '2000 hours', `
		if processedAgo > 0 {
			q += `now() - make_interval(secs => ?))`
			if err := e.db.Exec(q, id, ref, processedAgo.Seconds()).Error; err != nil {
				t.Fatal(err)
			}

			return
		}
		if err := e.db.Exec(q+`NULL)`, id, ref).Error; err != nil {
			t.Fatal(err)
		}
	}
	add("old-1", "r1", 800*time.Hour)
	add("old-2", "r2", 900*time.Hour)
	add("recent", "r3", time.Hour)
	add("unprocessed", "r4", 0)
	add("reviewed", review.ProviderRef(), 1000*time.Hour)

	if err := e.sweep(nil); err != nil {
		t.Fatal(err)
	}
	remaining := e.count(`SELECT count(*) FROM payment.webhook_events`)
	if remaining != 3 {
		t.Fatalf("%d notifications remain, want 3 (recent, unprocessed, reviewed)", remaining)
	}
	for _, id := range []string{"recent", "unprocessed", "reviewed"} {
		if e.count(`SELECT count(*) FROM payment.webhook_events WHERE provider_event_id = ?`, id) != 1 {
			t.Fatalf("sentinel %s was deleted", id)
		}
	}
}
