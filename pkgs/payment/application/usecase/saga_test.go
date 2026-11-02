package usecase_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
)

// recorder is both the outbox (port.EventPublisher) and the alert sink (port.Alerts) of a test: it
// keeps what was published and raised, in order.
type recorder struct {
	mu        sync.Mutex
	published []string // "<event>:<payment id>"
	webhooks  []string
	alerts    []string // "<alert>:<order id>"
}

func (r *recorder) Publish(_ context.Context, p *payment.Payment, events []payment.Event, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range events {
		r.published = append(r.published, string(e)+":"+p.ID().String())
	}

	return nil
}

func (r *recorder) PublishWebhookReceived(_ context.Context, provider, id string, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.webhooks = append(r.webhooks, provider+"/"+id)

	return nil
}

func (r *recorder) Raise(_ context.Context, a payment.Alert, orderID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.alerts = append(r.alerts, string(a)+":"+orderID)
}

func (r *recorder) events() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.published...)
}

func (r *recorder) raised() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.alerts...)
}

type sagaEnv struct {
	*env
	outcomes *memory.OutcomeRepository
	rec      *recorder
	ids      int
}

type seqIDs struct{ n *int }

func (s seqIDs) NewID() (string, error) {
	*s.n++

	return "pay-" + string(rune('a'+*s.n-1)), nil
}

func newSagaEnv(t *testing.T, fault func(op string) error) *sagaEnv {
	t.Helper()
	e := newEnv(t, fault)

	return &sagaEnv{env: e, outcomes: memory.NewOutcomeRepository(), rec: &recorder{}}
}

func (s *sagaEnv) confirmed() *usecase.RecordOrderConfirmed {
	return usecase.NewRecordOrderConfirmed(s.repo, s.outcomes, seqIDs{&s.ids}, s.clock, s.rec, 30*time.Minute)
}

func (s *sagaEnv) cancelled() *usecase.RecordOrderEnded {
	return usecase.NewRecordOrderCancelled(s.repo, s.outcomes, s.rec, s.clock)
}

func (s *sagaEnv) completed() *usecase.RecordOrderEnded {
	return usecase.NewRecordOrderCompleted(s.repo, s.outcomes, s.rec, s.clock)
}

func (s *sagaEnv) refund() *usecase.ProcessRefund {
	return usecase.NewProcessRefund(s.repo, s.prov, s.rec, memory.NewTransactor(), s.clock)
}

func total(amount int64) *dto.MoneyOutput { return &dto.MoneyOutput{Amount: amount, Currency: "TWD"} }

func confirmedIn(order string, tot *dto.MoneyOutput) dto.OrderConfirmedInput {
	return dto.OrderConfirmedInput{OrderID: order, CustomerID: owner, Total: tot}
}

func (s *sagaEnv) payment(t *testing.T, order string) *payment.Payment {
	t.Helper()
	p, err := s.repo.FindByOrderID(context.Background(), order)
	if err != nil {
		t.Fatalf("payment of %s: %v", order, err)
	}

	return p
}

// T1: OrderConfirmed creates a PENDING payment for the event's total, window from the clock, and the
// provider is not asked.
func TestRecordOrderConfirmed_CreatesAPendingPayment(t *testing.T) {
	s := newSagaEnv(t, nil)
	if _, err := s.confirmed().Execute(context.Background(), confirmedIn("ord-1", total(1500))); err != nil {
		t.Fatal(err)
	}
	p := s.payment(t, "ord-1")
	if p.Status() != payment.StatusPending || p.CustomerID() != owner || p.Amount().Amount() != 1500 || p.Amount().Currency() != "TWD" ||
		!p.ExpiresAt().Equal(t0.Add(30*time.Minute)) || p.ID() == "" || p.Outcome() != payment.OutcomeNone {
		t.Fatalf("created payment = %+v", p)
	}
	if s.calls(mock.OpCreateCheckout) != 0 || s.prov.Opened() != 0 {
		t.Fatal("creating the payment must not ask the provider (the checkout opens at StartPayment)")
	}
	if len(s.rec.events()) != 0 || len(s.rec.raised()) != 0 {
		t.Fatalf("T1 publishes nothing and alerts nobody: %v %v", s.rec.events(), s.rec.raised())
	}
}

// Redelivery and the second event of an order: one payment, untouched.
func TestRecordOrderConfirmed_AgainIsIgnored(t *testing.T) {
	s := newSagaEnv(t, nil)
	c := context.Background()
	if _, err := s.confirmed().Execute(c, confirmedIn("ord-1", total(1500))); err != nil {
		t.Fatal(err)
	}
	first := s.payment(t, "ord-1")
	if _, err := s.confirmed().Execute(c, confirmedIn("ord-1", total(9999))); err != nil {
		t.Fatalf("the same order again: %v", err)
	}
	again := s.payment(t, "ord-1")
	if again.ID() != first.ID() || again.Amount().Amount() != 1500 || again.Version() != first.Version() {
		t.Fatalf("payment changed by a repeat: %+v -> %+v", first, again)
	}
	if s.ids != 1 {
		t.Fatalf("%d ids generated, want 1: the repeat must not even look for a new payment", s.ids)
	}
}

// docs/payment.md §2: an OrderConfirmed without a total is never collected as 0. No payment row, an
// alert, a domain error (the consumer drops it) — and an existing payment is left alone.
func TestRecordOrderConfirmed_WithoutATotalIsRefused(t *testing.T) {
	c := context.Background()

	t.Run("no payment yet", func(t *testing.T) {
		s := newSagaEnv(t, nil)
		_, err := s.confirmed().Execute(c, confirmedIn("ord-1", nil))
		if !errors.Is(err, payment.ErrMissingTotal) {
			t.Fatalf("err = %v, want payment.missing_total", err)
		}
		if _, err = s.repo.FindByOrderID(c, "ord-1"); !errors.Is(err, payment.ErrNotFound) {
			t.Fatalf("a payment row exists for an order without a total: %v", err)
		}
		if got := s.rec.raised(); len(got) != 1 || got[0] != "missing_total:ord-1" {
			t.Fatalf("alerts = %v, want exactly [missing_total:ord-1]", got)
		}
	})

	t.Run("a payment already exists", func(t *testing.T) {
		s := newSagaEnv(t, nil)
		if _, err := s.confirmed().Execute(c, confirmedIn("ord-1", total(1500))); err != nil {
			t.Fatal(err)
		}
		before := s.payment(t, "ord-1")
		_, err := s.confirmed().Execute(c, confirmedIn("ord-1", nil))
		if !errors.Is(err, payment.ErrMissingTotal) {
			t.Fatalf("err = %v, want payment.missing_total", err)
		}
		after := s.payment(t, "ord-1")
		if after.Version() != before.Version() || after.Amount() != before.Amount() || after.Status() != before.Status() {
			t.Fatalf("the payment was touched: %+v -> %+v", before, after)
		}
		if got := s.rec.raised(); len(got) != 1 || got[0] != "missing_total:ord-1" {
			t.Fatalf("alerts = %v, want exactly [missing_total:ord-1]", got)
		}
	})

	t.Run("a total that is not money", func(t *testing.T) {
		s := newSagaEnv(t, nil)
		if _, err := s.confirmed().Execute(c, confirmedIn("ord-1", &dto.MoneyOutput{Amount: -5, Currency: "TWD"})); !errors.Is(err, payment.ErrInvalidAmount) {
			t.Fatalf("err = %v, want payment.invalid_amount", err)
		}
		if _, err := s.repo.FindByOrderID(c, "ord-1"); !errors.Is(err, payment.ErrNotFound) {
			t.Fatal("a payment row exists for an invalid total")
		}
	})

	t.Run("no order id", func(t *testing.T) {
		s := newSagaEnv(t, nil)
		if _, err := s.confirmed().Execute(c, confirmedIn("", total(5))); !errors.Is(err, payment.ErrInvalidOrderID) {
			t.Fatalf("err = %v", err)
		}
	})
}

// T15 + T14: the end of an order can arrive before its OrderConfirmed. The first word is remembered, and
// the payment is born ended: it cannot be paid.
func TestOutOfOrder_TheOutcomeIsRememberedUntilThePaymentExists(t *testing.T) {
	c := context.Background()
	for name, tc := range map[string]struct {
		end  func(*sagaEnv) *usecase.RecordOrderEnded
		want payment.Outcome
	}{
		"cancelled first": {(*sagaEnv).cancelled, payment.OutcomeCancelled},
		"completed first": {(*sagaEnv).completed, payment.OutcomeCompletedUnpaid},
	} {
		t.Run(name, func(t *testing.T) {
			s := newSagaEnv(t, nil)
			out, err := tc.end(s).Execute(c, dto.OrderEndedInput{OrderID: "ord-1"})
			if err != nil || out.CancelCheckoutRef != "" {
				t.Fatalf("end of an order without a payment: %+v, %v", out, err)
			}
			if got, _ := s.outcomes.Find(c, "ord-1"); got != tc.want {
				t.Fatalf("remembered outcome = %s, want %s", got, tc.want)
			}
			if _, err = s.repo.FindByOrderID(c, "ord-1"); !errors.Is(err, payment.ErrNotFound) {
				t.Fatal("the end of an order created a payment")
			}

			if _, err = s.confirmed().Execute(c, confirmedIn("ord-1", total(700))); err != nil {
				t.Fatal(err)
			}
			p := s.payment(t, "ord-1")
			if p.Status() != payment.StatusCancelled || p.Outcome() != tc.want || p.TerminalAt().IsZero() {
				t.Fatalf("payment born after the end = %s/%s terminal %v; want CANCELLED/%s", p.Status(), p.Outcome(), p.TerminalAt(), tc.want)
			}
			// and nobody can pay it
			if _, err = s.start.Execute(c, startIn(owner, false)); err != nil && !errors.Is(err, payment.ErrNotFound) && !errors.Is(err, payment.ErrNotPending) {
				t.Fatalf("StartPayment: %v", err)
			}
		})
	}

	t.Run("the first word stands", func(t *testing.T) {
		s := newSagaEnv(t, nil)
		_, _ = s.cancelled().Execute(c, dto.OrderEndedInput{OrderID: "ord-1"})
		_, _ = s.completed().Execute(c, dto.OrderEndedInput{OrderID: "ord-1"})
		if got, _ := s.outcomes.Find(c, "ord-1"); got != payment.OutcomeCancelled {
			t.Fatalf("outcome = %s, want the first (CANCELLED)", got)
		}
	})
}

// seedPaid stores a PENDING payment of order straight into the repository (the fixture must not depend on
// the use case under test) and, with startCheckout, opens its checkout through StartPayment.
func (s *sagaEnv) seedPaid(t *testing.T, order string, startCheckout bool) {
	t.Helper()
	c := context.Background()
	p, err := payment.New(payment.NewParams{ID: payment.ID("pay-" + order), OrderID: order, CustomerID: owner, Total: money(t), ExpiresAt: t0.Add(30 * time.Minute)}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.repo.Save(c, p); err != nil {
		t.Fatal(err)
	}
	if startCheckout {
		if _, err = s.start.Execute(c, dto.StartPaymentInput{OrderID: order, CustomerID: owner}); err != nil {
			t.Fatal(err)
		}
	}
}

// T7 / T7' / T10 / T12 / T13 through the use case: state, outbox, and the checkout to cancel.
func TestRecordOrderEnded_Transitions(t *testing.T) {
	c := context.Background()

	t.Run("PENDING cancelled with a checkout open: CANCELLED and the checkout is named", func(t *testing.T) {
		s := newSagaEnv(t, nil)
		s.seedPaid(t, orderID, true)
		ref := s.payment(t, orderID).ProviderRef()
		out, err := s.cancelled().Execute(c, dto.OrderEndedInput{OrderID: orderID})
		if err != nil || out.CancelCheckoutRef != ref || ref == "" {
			t.Fatalf("out = %+v err = %v, want the checkout %q", out, err, ref)
		}
		if p := s.payment(t, orderID); p.Status() != payment.StatusCancelled || p.Outcome() != payment.OutcomeCancelled || p.TerminalAt().IsZero() {
			t.Fatalf("payment = %s/%s", p.Status(), p.Outcome())
		}
	})

	t.Run("PENDING cancelled before any checkout: nothing to cancel", func(t *testing.T) {
		s := newSagaEnv(t, nil)
		s.seedPaid(t, orderID, false)
		out, err := s.cancelled().Execute(c, dto.OrderEndedInput{OrderID: orderID})
		if err != nil || out.CancelCheckoutRef != "" {
			t.Fatalf("out = %+v err = %v", out, err)
		}
	})

	t.Run("PENDING completed without the payment: CANCELLED / COMPLETED_UNPAID (T7')", func(t *testing.T) {
		s := newSagaEnv(t, nil)
		s.seedPaid(t, orderID, true)
		out, err := s.completed().Execute(c, dto.OrderEndedInput{OrderID: orderID})
		if err != nil || out.CancelCheckoutRef == "" {
			t.Fatalf("out = %+v err = %v, want the open checkout named", out, err)
		}
		if p := s.payment(t, orderID); p.Status() != payment.StatusCancelled || p.Outcome() != payment.OutcomeCompletedUnpaid {
			t.Fatalf("payment = %s/%s", p.Status(), p.Outcome())
		}
	})

	capture := func(t *testing.T, s *sagaEnv) {
		t.Helper()
		p := s.payment(t, orderID)
		p.Capture(payment.SourceWebhook, p.Amount(), t0)
		if err := s.repo.Save(c, p); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("CAPTURED then cancelled: REFUNDING and PaymentRefundRequested (T10)", func(t *testing.T) {
		s := newSagaEnv(t, nil)
		s.seedPaid(t, orderID, true)
		capture(t, s)
		if _, err := s.cancelled().Execute(c, dto.OrderEndedInput{OrderID: orderID}); err != nil {
			t.Fatal(err)
		}
		p := s.payment(t, orderID)
		if p.Status() != payment.StatusRefunding || p.Outcome() != payment.OutcomeCancelled {
			t.Fatalf("payment = %s/%s", p.Status(), p.Outcome())
		}
		if got := s.rec.events(); len(got) != 1 || got[0] != "PaymentRefundRequested:"+p.ID().String() {
			t.Fatalf("outbox = %v, want exactly the refund request", got)
		}
	})

	t.Run("CAPTURED then completed: the normal path, nothing changes and nobody is refunded", func(t *testing.T) {
		s := newSagaEnv(t, nil)
		s.seedPaid(t, orderID, true)
		capture(t, s)
		if _, err := s.completed().Execute(c, dto.OrderEndedInput{OrderID: orderID}); err != nil {
			t.Fatal(err)
		}
		after := s.payment(t, orderID)
		if after.Status() != payment.StatusCaptured || after.Outcome() != payment.OutcomeNone {
			t.Fatalf("payment = %s/%s, want CAPTURED/NONE (order_outcome stays NONE: a late OrderCancelled must still refund)", after.Status(), after.Outcome())
		}
		if len(s.rec.events()) != 0 {
			t.Fatalf("outbox = %v, want nothing", s.rec.events())
		}
	})

	t.Run("a repeat on CANCELLED changes nothing (T13)", func(t *testing.T) {
		s := newSagaEnv(t, nil)
		s.seedPaid(t, orderID, false)
		_, _ = s.cancelled().Execute(c, dto.OrderEndedInput{OrderID: orderID})
		before := s.payment(t, orderID)
		out, err := s.cancelled().Execute(c, dto.OrderEndedInput{OrderID: orderID})
		after := s.payment(t, orderID)
		if err != nil || out.CancelCheckoutRef != "" {
			t.Fatalf("repeat: out %+v err %v", out, err)
		}
		if after.Status() != payment.StatusCancelled || after.Outcome() != before.Outcome() || !after.TerminalAt().Equal(before.TerminalAt()) {
			t.Fatalf("repeat changed the payment: %s/%s/%v -> %s/%s/%v", before.Status(), before.Outcome(), before.TerminalAt(), after.Status(), after.Outcome(), after.TerminalAt())
		}
		if len(s.rec.events()) != 0 {
			t.Fatalf("repeat published %v", s.rec.events())
		}
	})

	t.Run("no order id", func(t *testing.T) {
		s := newSagaEnv(t, nil)
		if _, err := s.cancelled().Execute(c, dto.OrderEndedInput{}); !errors.Is(err, payment.ErrInvalidOrderID) {
			t.Fatalf("err = %v", err)
		}
	})
}

// The refund call (T11): idempotent at the provider on the payment id, bounded failure leaves the payment
// REFUNDING for the retry, and the provider's words never come back.
func TestProcessRefund(t *testing.T) {
	const marker = "SECRET-REFUND-MARKER-71c2"
	c := context.Background()

	refunding := func(t *testing.T, s *sagaEnv) *payment.Payment {
		t.Helper()
		s.seedPaid(t, orderID, true)
		p := s.payment(t, orderID)
		if _, err := s.prov.Pay(p.ProviderRef(), p.Amount()); err != nil {
			t.Fatal(err)
		}
		p.Capture(payment.SourceWebhook, p.Amount(), t0)
		p.OrderCancelled(t0)
		if err := s.repo.Save(c, p); err != nil {
			t.Fatal(err)
		}
		if got := s.payment(t, orderID).Status(); got != payment.StatusRefunding {
			t.Fatalf("fixture status = %s", got)
		}

		return s.payment(t, orderID)
	}

	t.Run("success: REFUNDED, PaymentRefunded, one refund at the provider; a repeat refunds nothing twice", func(t *testing.T) {
		s := newSagaEnv(t, nil)
		p := refunding(t, s)
		if _, err := s.refund().Execute(c, dto.RefundInput{PaymentID: p.ID().String()}); err != nil {
			t.Fatal(err)
		}
		if got := s.payment(t, orderID); got.Status() != payment.StatusRefunded {
			t.Fatalf("status = %s", got.Status())
		}
		if ev := s.rec.events(); len(ev) != 1 || ev[0] != "PaymentRefunded:"+p.ID().String() {
			t.Fatalf("outbox = %v", ev)
		}
		if s.calls(mock.OpRefund) != 1 || s.prov.Refunds(p.ProviderRef()) != 1 {
			t.Fatalf("provider refund calls %d, keys %d; want 1 and 1", s.calls(mock.OpRefund), s.prov.Refunds(p.ProviderRef()))
		}
		if _, err := s.refund().Execute(c, dto.RefundInput{PaymentID: p.ID().String()}); err != nil {
			t.Fatalf("repeat: %v", err)
		}
		if s.calls(mock.OpRefund) != 1 || len(s.rec.events()) != 1 {
			t.Fatalf("a repeat asked the provider again (%d calls) or published again (%v)", s.calls(mock.OpRefund), s.rec.events())
		}
	})

	t.Run("a refund that always fails: REFUNDING stays, fixed error, no words of the provider; recovery converges", func(t *testing.T) {
		down := true
		s := newSagaEnv(t, nil)
		// the failing provider is a wrapper whose Refund returns text with a marker in it
		leaky := &leakyRefund{Provider: s.prov, marker: marker, fail: &down}
		uc := usecase.NewProcessRefund(s.repo, leaky, s.rec, memory.NewTransactor(), s.clock)
		p := refunding(t, s)

		for range 3 {
			_, err := uc.Execute(c, dto.RefundInput{PaymentID: p.ID().String()})
			if !errors.Is(err, port.ErrProviderUnavailable) || strings.Contains(err.Error(), marker) {
				t.Fatalf("err = %v, want only the fixed sentinel", err)
			}
		}
		if leaky.calls != 3 || !strings.Contains(leaky.said, marker) {
			t.Fatalf("positive control: refund called %d times, said %q", leaky.calls, leaky.said)
		}
		if got := s.payment(t, orderID); got.Status() != payment.StatusRefunding {
			t.Fatalf("status = %s, want REFUNDING while the refund fails", got.Status())
		}
		if len(s.rec.events()) != 0 {
			t.Fatalf("outbox = %v while failing", s.rec.events())
		}

		down = false
		if _, err := uc.Execute(c, dto.RefundInput{PaymentID: p.ID().String()}); err != nil {
			t.Fatalf("after recovery: %v", err)
		}
		if got := s.payment(t, orderID); got.Status() != payment.StatusRefunded {
			t.Fatalf("status = %s after recovery, want REFUNDED", got.Status())
		}
	})

	t.Run("not refunding / unknown / empty", func(t *testing.T) {
		s := newSagaEnv(t, nil)
		s.seedPaid(t, orderID, true)
		p := s.payment(t, orderID)
		if _, err := s.refund().Execute(c, dto.RefundInput{PaymentID: p.ID().String()}); !errors.Is(err, payment.ErrNotRefunding) {
			t.Fatalf("PENDING: %v", err)
		}
		if _, err := s.refund().Execute(c, dto.RefundInput{PaymentID: "nope"}); !errors.Is(err, payment.ErrNotFound) {
			t.Fatalf("unknown: %v", err)
		}
		if _, err := s.refund().Execute(c, dto.RefundInput{}); !errors.Is(err, payment.ErrInvalidID) {
			t.Fatalf("empty: %v", err)
		}
		if s.calls(mock.OpRefund) != 0 {
			t.Fatal("the provider was asked to refund a payment that is not REFUNDING")
		}
	})

	t.Run("someone else moved it meanwhile: a lost race is retried, not dropped", func(t *testing.T) {
		s := newSagaEnv(t, nil)
		p := refunding(t, s)
		race := &raceOnRefund{Provider: s.prov, onRefund: func() {
			q := s.payment(t, orderID)
			if _, err := q.RefundSucceeded(t0); err != nil {
				t.Error(err)
			}
			if err := s.repo.Save(c, q); err != nil {
				t.Error(err)
			}
		}}
		uc := usecase.NewProcessRefund(s.repo, race, s.rec, memory.NewTransactor(), s.clock)
		_, err := uc.Execute(c, dto.RefundInput{PaymentID: p.ID().String()})
		if !errors.Is(err, payment.ErrStale) {
			t.Fatalf("err = %v, want ErrStale (retry reloads and finds REFUNDED)", err)
		}
		if _, err = s.refund().Execute(c, dto.RefundInput{PaymentID: p.ID().String()}); err != nil {
			t.Fatalf("the retry: %v", err)
		}
	})
}

// leakyRefund fails Refund with text carrying marker while *fail, and keeps count and text.
type leakyRefund struct {
	*mock.Provider
	marker string
	fail   *bool
	calls  int
	said   string
}

func (l *leakyRefund) Refund(c context.Context, in dto.ProviderRefundInput) error {
	if *l.fail {
		l.calls++
		l.said = "gateway refused key " + l.marker
		return errors.New(l.said)
	}

	return l.Provider.Refund(c, in)
}

type raceOnRefund struct {
	*mock.Provider
	onRefund func()
}

func (r *raceOnRefund) Refund(c context.Context, in dto.ProviderRefundInput) error {
	err := r.Provider.Refund(c, in)
	if f := r.onRefund; f != nil {
		r.onRefund = nil
		f()
	}

	return err
}
