package usecase_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
)

const (
	reconcileWindow = 24 * time.Hour
	sweepInterval   = 15 * time.Minute
	retention       = 720 * time.Hour
)

type sweepEnv struct {
	*webhookEnv
	failQuery func() bool
}

// newSweepEnv: a provider whose GetPayment can be made to fail (counted through Fault, like every other op).
func newSweepEnv(t *testing.T) *sweepEnv {
	t.Helper()
	s := &sweepEnv{}
	s.webhookEnv = &webhookEnv{webhooks: memory.NewWebhookRepository(nil)}
	s.sagaEnv = newSagaEnv(t, func(op string) error {
		if op == mock.OpGetPayment && s.failQuery != nil && s.failQuery() {
			return errors.New("gateway says SECRET-QUERY-MARKER")
		}

		return nil
	})

	return s
}

func (s *sweepEnv) sweep() *usecase.ExpireSweep {
	return usecase.NewExpireSweep(s.repo, s.webhooks, s.prov, s.rec, s.rec, memory.NewTransactor(), s.clock, dto.SweepConfig{
		ReconcileWindow: reconcileWindow, Interval: sweepInterval, Batch: 100, WebhookRetention: retention,
	})
}

func (s *sweepEnv) run(t *testing.T) *dto.ExpireOutput {
	t.Helper()
	out, err := s.sweep().Execute(context.Background(), dto.Ack{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	return out
}

// pastWindow moves the clock to just after the payment window (30 minutes) closed.
func (s *sweepEnv) pastWindow() { s.clock.Advance(31 * time.Minute) }

// T5: the window closed and the provider has no money: EXPIRED and PaymentExpired. The provider was asked first.
func TestSweep_ExpiresAPaymentThatWasNotPaid(t *testing.T) {
	s := newSweepEnv(t)
	s.seedPaid(t, orderID, true)
	s.pastWindow()

	out := s.run(t)
	p := s.payment(t, orderID)
	if p.Status() != payment.StatusExpired || p.TerminalAt().IsZero() || out.Expired != 1 {
		t.Fatalf("status %s terminal %v expired %d", p.Status(), p.TerminalAt(), out.Expired)
	}
	if ev := s.rec.events(); len(ev) != 1 || ev[0] != "PaymentExpired:"+p.ID().String() {
		t.Fatalf("outbox = %v", ev)
	}
	if s.calls(mock.OpGetPayment) != 1 {
		t.Fatalf("the provider was asked %d times, want 1 (a payment with a checkout is never expired unasked)", s.calls(mock.OpGetPayment))
	}
	// a second run has nothing to do
	if again := s.run(t); again.Expired != 0 || len(s.rec.events()) != 1 {
		t.Fatalf("second run: %+v events %v", again, s.rec.events())
	}
}

func TestSweep_LeavesWhatIsNotDue(t *testing.T) {
	s := newSweepEnv(t)
	s.seedPaid(t, orderID, true) // window still open
	out := s.run(t)
	if s.payment(t, orderID).Status() != payment.StatusPending || out.Expired != 0 || s.calls(mock.OpGetPayment) != 0 {
		t.Fatalf("a payment inside its window was touched or its provider asked (%d calls)", s.calls(mock.OpGetPayment))
	}
}

// A payment that never opened a checkout cannot have been paid: no query, straight to EXPIRED.
func TestSweep_ExpiresACheckoutlessPaymentWithoutAskingTheProvider(t *testing.T) {
	s := newSweepEnv(t)
	s.seedPaid(t, orderID, false)
	s.pastWindow()
	s.run(t)
	if s.payment(t, orderID).Status() != payment.StatusExpired || s.calls(mock.OpGetPayment) != 0 {
		t.Fatalf("status %s, provider asked %d times", s.payment(t, orderID).Status(), s.calls(mock.OpGetPayment))
	}
}

// T6 / T6': the webhook never came, the sweep finds the money.
func TestSweep_FindsMoneyTheWebhookNeverReported(t *testing.T) {
	t.Run("matching: CAPTURED and PaymentCaptured, not EXPIRED", func(t *testing.T) {
		s := newSweepEnv(t)
		s.seedPaid(t, orderID, true)
		s.paid(t, orderID, twd(t, 1000))
		s.pastWindow()
		out := s.run(t)
		p := s.payment(t, orderID)
		if p.Status() != payment.StatusCaptured || out.Captured != 1 || out.Expired != 0 {
			t.Fatalf("status %s %+v", p.Status(), out)
		}
		if ev := s.rec.events(); len(ev) != 1 || ev[0] != "PaymentCaptured:"+p.ID().String() {
			t.Fatalf("outbox = %v: a paid payment must never publish PaymentExpired", ev)
		}
	})
	t.Run("not matching: NEEDS_REVIEW with an alert and no PaymentCaptured", func(t *testing.T) {
		s := newSweepEnv(t)
		s.seedPaid(t, orderID, true)
		s.paid(t, orderID, twd(t, 999))
		s.pastWindow()
		out := s.run(t)
		if s.payment(t, orderID).Status() != payment.StatusNeedsReview || out.NeedsReview != 1 || len(s.rec.events()) != 0 {
			t.Fatalf("status %s %+v events %v", s.payment(t, orderID).Status(), out, s.rec.events())
		}
		if got := s.rec.raised(); len(got) != 1 || got[0] != "amount_mismatch:"+orderID {
			t.Fatalf("alerts = %v", got)
		}
	})
}

// §3 failure table "expire 查 provider 失敗": a query that fails moves NOTHING (never EXPIRED on "could not ask"),
// is counted, is retried by the next run and converges after recovery; several runs in a row alert "stuck".
func TestSweep_AFailedQueryMovesNothingAndConvergesAfterRecovery(t *testing.T) {
	s := newSweepEnv(t)
	down := true
	s.failQuery = func() bool { return down }
	s.seedPaid(t, orderID, true)
	s.pastWindow()

	for i := range 2 {
		out := s.run(t)
		if s.payment(t, orderID).Status() != payment.StatusPending || out.Expired != 0 || out.QueryFailures != 1 {
			t.Fatalf("run %d: status %s %+v; want PENDING, 1 failure", i, s.payment(t, orderID).Status(), out)
		}
		if len(s.rec.events()) != 0 {
			t.Fatalf("run %d published %v on a failed query", i, s.rec.events())
		}
	}
	if s.calls(mock.OpGetPayment) != 2 {
		t.Fatalf("positive control: %d queries, want 2 (one per run)", s.calls(mock.OpGetPayment))
	}
	if len(s.rec.raised()) != 0 {
		t.Fatalf("alerts after 2 sweeps = %v, want none yet", s.rec.raised())
	}

	// three sweeps' worth of overdue: stuck
	s.clock.Advance(stuckAfter())
	s.run(t)
	if got := s.rec.raised(); len(got) != 1 || got[0] != "provider_query_stuck:"+orderID {
		t.Fatalf("alerts = %v, want one provider_query_stuck", got)
	}
	if s.payment(t, orderID).Status() != payment.StatusPending {
		t.Fatal("a stuck payment must still be PENDING")
	}

	down = false
	if out := s.run(t); out.Expired != 1 || s.payment(t, orderID).Status() != payment.StatusExpired {
		t.Fatalf("after recovery: %+v status %s", out, s.payment(t, orderID).Status())
	}
}

func stuckAfter() time.Duration { return 3 * sweepInterval }

// §3 補查有界 and T8 / T9 / T10: an EXPIRED or CANCELLED payment is asked again until its window closes, because
// cancelling a checkout may have failed and the webhook may never arrive.
func TestSweep_ReconcilesWithinTheWindow(t *testing.T) {
	c := context.Background()

	t.Run("T8: expired, then the money appears: CAPTURED", func(t *testing.T) {
		s := newSweepEnv(t)
		s.seedPaid(t, orderID, true)
		d := s.paid(t, orderID, twd(t, 1000))
		_ = d
		s.pastWindow()
		// expire it without the money showing: ask while the provider says unpaid... then it pays
		p := s.payment(t, orderID)
		p.Unpaid(s.clock.Now())
		if err := s.repo.Save(c, p); err != nil {
			t.Fatal(err)
		}
		out := s.run(t)
		if got := s.payment(t, orderID).Status(); got != payment.StatusCaptured || out.Captured != 1 {
			t.Fatalf("status %s %+v", got, out)
		}
		if ev := s.rec.events(); len(ev) != 1 || !strings.HasPrefix(ev[0], "PaymentCaptured:") {
			t.Fatalf("outbox = %v", ev)
		}
	})

	t.Run("T9: expired, money that does not match: NEEDS_REVIEW", func(t *testing.T) {
		s := newSweepEnv(t)
		s.seedPaid(t, orderID, true)
		s.paid(t, orderID, twd(t, 1))
		p := s.payment(t, orderID)
		p.Unpaid(s.clock.Now().Add(time.Hour))
		if err := s.repo.Save(c, p); err != nil {
			t.Fatal(err)
		}
		s.clock.Advance(time.Hour + time.Minute)
		s.run(t)
		if got := s.payment(t, orderID).Status(); got != payment.StatusNeedsReview {
			t.Fatalf("status %s", got)
		}
	})

	t.Run("cancelled whose checkout could not be cancelled, then paid: captured and refunded (T8 -> T10)", func(t *testing.T) {
		s := newSweepEnv(t)
		s.seedPaid(t, orderID, true)
		s.paid(t, orderID, twd(t, 1000))
		if _, err := s.cancelled().Execute(c, dto.OrderEndedInput{OrderID: orderID}); err != nil { // the cancel at the provider "failed": nothing cancelled it
			t.Fatal(err)
		}
		s.clock.Advance(time.Hour)
		s.run(t)
		p := s.payment(t, orderID)
		if p.Status() != payment.StatusRefunding {
			t.Fatalf("status %s, want REFUNDING", p.Status())
		}
		ev := s.rec.events()
		if len(ev) != 2 || !strings.HasPrefix(ev[0], "PaymentCaptured:") || !strings.HasPrefix(ev[1], "PaymentRefundRequested:") {
			t.Fatalf("outbox = %v", ev)
		}
	})

	t.Run("nothing paid: asked, left alone", func(t *testing.T) {
		s := newSweepEnv(t)
		s.seedPaid(t, orderID, true)
		_, _ = s.cancelled().Execute(c, dto.OrderEndedInput{OrderID: orderID})
		s.clock.Advance(time.Hour)
		out := s.run(t)
		if s.payment(t, orderID).Status() != payment.StatusCancelled || out.Captured != 0 || s.calls(mock.OpGetPayment) != 1 {
			t.Fatalf("status %s %+v asked %d", s.payment(t, orderID).Status(), out, s.calls(mock.OpGetPayment))
		}
	})

	t.Run("the window is bounded: after it, no more queries and no state change (money paid late stays unseen: 已知限制)", func(t *testing.T) {
		s := newSweepEnv(t)
		s.seedPaid(t, orderID, true)
		s.paid(t, orderID, twd(t, 1000))
		_, _ = s.cancelled().Execute(c, dto.OrderEndedInput{OrderID: orderID})
		s.clock.Advance(reconcileWindow + time.Second)
		s.run(t)
		if s.calls(mock.OpGetPayment) != 0 || s.payment(t, orderID).Status() != payment.StatusCancelled {
			t.Fatalf("after the window: %d queries, status %s", s.calls(mock.OpGetPayment), s.payment(t, orderID).Status())
		}
	})

	t.Run("the last second of the window is still inside it", func(t *testing.T) {
		s := newSweepEnv(t)
		s.seedPaid(t, orderID, true)
		s.paid(t, orderID, twd(t, 1000))
		_, _ = s.cancelled().Execute(c, dto.OrderEndedInput{OrderID: orderID})
		s.clock.Advance(reconcileWindow - time.Second)
		s.run(t)
		if s.payment(t, orderID).Status() != payment.StatusRefunding {
			t.Fatalf("status %s, want the late money found one second before the deadline", s.payment(t, orderID).Status())
		}
	})
}

// §3 failure table, "expire 查 provider 失敗" for EXPIRED / CANCELLED: nothing moves, the next run retries inside the
// window, and the LAST query that fails is alerted ("補查未完成").
func TestSweep_AFailedReconcileQueryIsRetriedAndTheLastOneIsAlerted(t *testing.T) {
	c := context.Background()
	s := newSweepEnv(t)
	down := true
	s.failQuery = func() bool { return down }
	s.seedPaid(t, orderID, true)
	_, _ = s.cancelled().Execute(c, dto.OrderEndedInput{OrderID: orderID})

	// early in the window: a failure is just retried
	s.clock.Advance(time.Hour)
	out := s.run(t)
	if out.QueryFailures != 1 || s.payment(t, orderID).Status() != payment.StatusCancelled || len(s.rec.raised()) != 0 {
		t.Fatalf("early failure: %+v status %s alerts %v", out, s.payment(t, orderID).Status(), s.rec.raised())
	}
	if s.calls(mock.OpGetPayment) != 1 {
		t.Fatalf("positive control: %d queries", s.calls(mock.OpGetPayment))
	}

	// the run after which the window will have closed: the failure is the payment's last chance
	deadline := s.payment(t, orderID).ReconcileDeadline(reconcileWindow)
	s.clock.Advance(deadline.Add(-sweepInterval / 2).Sub(s.clock.Now()))
	s.run(t)
	if got := s.rec.raised(); len(got) != 1 || got[0] != "reconcile_incomplete:"+orderID {
		t.Fatalf("alerts = %v, want one reconcile_incomplete", got)
	}
}

// NEEDS_REVIEW is the operator's: the sweep neither asks the provider about it nor moves it.
func TestSweep_NeverTouchesNeedsReview(t *testing.T) {
	c := context.Background()
	s := newSweepEnv(t)
	s.seedPaid(t, orderID, true)
	p := s.payment(t, orderID)
	p.Capture(payment.SourceWebhook, twd(t, 1), s.clock.Now()) // mismatch -> NEEDS_REVIEW
	if err := s.repo.Save(c, p); err != nil {
		t.Fatal(err)
	}
	before := s.payment(t, orderID)
	s.clock.Advance(40 * time.Hour)
	s.run(t)
	after := s.payment(t, orderID)
	if after.Status() != payment.StatusNeedsReview || after.Version() != before.Version() || s.calls(mock.OpGetPayment) != 0 {
		t.Fatalf("status %s version %d -> %d, queries %d", after.Status(), before.Version(), after.Version(), s.calls(mock.OpGetPayment))
	}
}

// A payment someone else changed meanwhile is skipped and counted, the rest of the run still happens, and any
// other write failure fails the run at its end — without a word of the cause.
func TestSweep_OnePaymentsTroubleDoesNotStopTheRest(t *testing.T) {
	c := context.Background()
	s := newSweepEnv(t)
	s.seedPaid(t, "ord-a", false)
	s.seedPaid(t, "ord-b", false)
	s.pastWindow()

	t.Run("a lost lock", func(t *testing.T) {
		repo := &saveFails{Repository: s.repo, failFor: "ord-a", err: payment.ErrStale}
		sweep := usecase.NewExpireSweep(repo, s.webhooks, s.prov, s.rec, s.rec, memory.NewTransactor(), s.clock, dto.SweepConfig{ReconcileWindow: reconcileWindow, Interval: sweepInterval, Batch: 100})
		out, err := sweep.Execute(c, dto.Ack{})
		if err != nil || out.Conflicts != 1 || out.Expired != 1 {
			t.Fatalf("out %+v err %v; want one conflict and one expired", out, err)
		}
		if s.payment(t, "ord-a").Status() != payment.StatusPending || s.payment(t, "ord-b").Status() != payment.StatusExpired {
			t.Fatal("ord-a should still be PENDING and ord-b EXPIRED")
		}
	})

	t.Run("a broken write", func(t *testing.T) {
		repo := &saveFails{Repository: s.repo, failFor: "ord-a", err: errors.New("disk on fire SECRET-WRITE-MARKER")}
		sweep := usecase.NewExpireSweep(repo, s.webhooks, s.prov, s.rec, s.rec, memory.NewTransactor(), s.clock, dto.SweepConfig{ReconcileWindow: reconcileWindow, Interval: sweepInterval, Batch: 100})
		out, err := sweep.Execute(c, dto.Ack{})
		if err == nil || out.Failures != 1 || strings.Contains(err.Error(), "SECRET-WRITE-MARKER") {
			t.Fatalf("out %+v err %v; want the run to fail with a count and no cause", out, err)
		}
	})
}

type saveFails struct {
	payment.Repository
	failFor string
	err     error
}

func (s *saveFails) Save(c context.Context, p *payment.Payment) error {
	if p.OrderID() == s.failFor {
		return s.err
	}

	return s.Repository.Save(c, p)
}

// Retention (docs/payment.md §9 / the lead's conditions): processed notifications older than payment.webhook_retention
// go, in a bounded batch, and ONLY those: unprocessed, recent, and the ones of a payment under review stay. Its failure
// changes nothing of the run's transitions. 0 keeps everything.
func TestSweep_RetentionDeletesOnlyOldProcessedNotifications(t *testing.T) {
	c := context.Background()
	s := newSweepEnv(t)
	review := newPaymentUnderReview(t, s)

	insert := func(id, ref string, processedAgo time.Duration) {
		t.Helper()
		w, err := payment.NewWebhookEvent("mock", id, payment.WebhookCaptured, ref, twd(t, 1), []byte("{}"), s.clock.Now())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.webhooks.Insert(c, w); err != nil {
			t.Fatal(err)
		}
		if processedAgo > 0 {
			if err = s.webhooks.MarkProcessed(c, "mock", id, s.clock.Now().Add(-processedAgo)); err != nil {
				t.Fatal(err)
			}
		}
	}
	insert("old-1", "r1", retention+time.Hour)
	insert("old-2", "r2", retention+2*time.Hour)
	insert("recent", "r3", time.Hour)                               // processed, inside the retention
	insert("unprocessed", "r4", 0)                                  // never processed: kept whatever its age
	insert("reviewed", review.ProviderRef(), retention+3*time.Hour) // old and processed, but its payment is under review

	out := s.run(t)
	if out.RetentionDeleted != 2 || out.RetentionFailed {
		t.Fatalf("retention = %+v, want 2 deleted", out)
	}
	for _, id := range []string{"old-1", "old-2"} {
		if _, err := s.webhooks.Find(c, "mock", id); !errors.Is(err, payment.ErrNotFound) {
			t.Fatalf("%s survived retention: %v", id, err)
		}
	}
	for _, id := range []string{"recent", "unprocessed", "reviewed"} {
		if _, err := s.webhooks.Find(c, "mock", id); err != nil {
			t.Fatalf("sentinel %s was deleted: %v", id, err)
		}
	}

	t.Run("0 keeps everything", func(t *testing.T) {
		s2 := newSweepEnv(t)
		w, _ := payment.NewWebhookEvent("mock", "old", payment.WebhookCaptured, "r", twd(t, 1), []byte("{}"), s2.clock.Now())
		_, _ = s2.webhooks.Insert(c, w)
		_ = s2.webhooks.MarkProcessed(c, "mock", "old", s2.clock.Now().Add(-10*retention))
		sweep := usecase.NewExpireSweep(s2.repo, s2.webhooks, s2.prov, s2.rec, s2.rec, memory.NewTransactor(), s2.clock, dto.SweepConfig{ReconcileWindow: reconcileWindow, Interval: sweepInterval, Batch: 100})
		if out, err := sweep.Execute(c, dto.Ack{}); err != nil || out.RetentionDeleted != 0 {
			t.Fatalf("out %+v err %v", out, err)
		}
		if _, err := s2.webhooks.Find(c, "mock", "old"); err != nil {
			t.Fatalf("a retention of 0 deleted a notification: %v", err)
		}
	})

	t.Run("a failing delete does not stop the transitions of the run", func(t *testing.T) {
		s3 := newSweepEnv(t)
		s3.seedPaid(t, orderID, false)
		s3.pastWindow()
		sweep := usecase.NewExpireSweep(s3.repo, deleteFails{s3.webhooks}, s3.prov, s3.rec, s3.rec, memory.NewTransactor(), s3.clock, dto.SweepConfig{
			ReconcileWindow: reconcileWindow, Interval: sweepInterval, Batch: 100, WebhookRetention: retention,
		})
		out, err := sweep.Execute(c, dto.Ack{})
		if err != nil || !out.RetentionFailed || out.Expired != 1 {
			t.Fatalf("out %+v err %v; want RetentionFailed and the payment still expired", out, err)
		}
		if s3.payment(t, orderID).Status() != payment.StatusExpired {
			t.Fatal("the payment was not expired because retention failed")
		}
	})

	t.Run("the batch bounds the delete", func(t *testing.T) {
		s4 := newSweepEnv(t)
		for _, id := range []string{"a", "b", "c"} {
			w, _ := payment.NewWebhookEvent("mock", id, payment.WebhookCaptured, "r-"+id, twd(t, 1), []byte("{}"), s4.clock.Now())
			_, _ = s4.webhooks.Insert(c, w)
			_ = s4.webhooks.MarkProcessed(c, "mock", id, s4.clock.Now().Add(-2*retention))
		}
		sweep := usecase.NewExpireSweep(s4.repo, s4.webhooks, s4.prov, s4.rec, s4.rec, memory.NewTransactor(), s4.clock, dto.SweepConfig{
			ReconcileWindow: reconcileWindow, Interval: sweepInterval, Batch: 2, WebhookRetention: retention,
		})
		if out, err := sweep.Execute(c, dto.Ack{}); err != nil || out.RetentionDeleted != 2 {
			t.Fatalf("out %+v err %v; want 2 (the batch)", out, err)
		}
	})
}

type deleteFails struct{ payment.WebhookRepository }

func (deleteFails) DeleteProcessedBefore(context.Context, time.Time, int) (int, error) {
	return 0, errors.New("could not delete")
}

// newPaymentUnderReview stores a NEEDS_REVIEW payment with a checkout and wires the webhook repository to know it.
func newPaymentUnderReview(t *testing.T, s *sweepEnv) *payment.Payment {
	t.Helper()
	c := context.Background()
	s.seedPaid(t, "ord-review", true)
	p := s.payment(t, "ord-review")
	p.Capture(payment.SourceWebhook, twd(t, 1), s.clock.Now())
	if err := s.repo.Save(c, p); err != nil {
		t.Fatal(err)
	}
	s.webhooks = memory.NewWebhookRepository(func(provider, ref string) bool {
		q, err := s.repo.FindByProviderRef(c, provider, ref)

		return err == nil && q.Status() == payment.StatusNeedsReview
	})

	return s.payment(t, "ord-review")
}

// docs/payment.md §9「查詢持續失敗的完整序列」: the provider stays down for the whole life of a payment. The sweep
// moves nothing while it is down (PENDING stays PENDING); the order's own TTL then cancels the order, which
// reaches payment as OrderCancelled (T7: CANCELLED, the checkout stays open at the provider); the reconcile
// keeps failing without moving anything; once the provider answers inside the window it finds the money
// that was paid meanwhile and sends it back (T8 -> T10). Nothing is lost at any point of the sequence.
func TestSweep_TheWholeSequenceWhenTheProviderStaysDown(t *testing.T) {
	c := context.Background()
	s := newSweepEnv(t)
	down := true
	s.failQuery = func() bool { return down }
	s.seedPaid(t, orderID, true)
	paidAt := s.paid(t, orderID, twd(t, 1000)) // the customer paid on the hosted page...
	_ = paidAt                                 // ...and the webhook never came

	// 1. the window closes; the provider is down: nothing moves, however often it is tried
	s.pastWindow()
	for range 2 {
		out := s.run(t)
		if out.QueryFailures != 1 || s.payment(t, orderID).Status() != payment.StatusPending {
			t.Fatalf("while down: %+v status %s", out, s.payment(t, orderID).Status())
		}
	}
	if len(s.rec.events()) != 0 {
		t.Fatalf("events while down: %v", s.rec.events())
	}

	// 2. the reservation TTL cancels the order; OrderCancelled reaches payment: T7
	if _, err := s.cancelled().Execute(c, dto.OrderEndedInput{OrderID: orderID}); err != nil {
		t.Fatal(err)
	}
	p := s.payment(t, orderID)
	if p.Status() != payment.StatusCancelled || p.Outcome() != payment.OutcomeCancelled {
		t.Fatalf("after OrderCancelled: %s/%s", p.Status(), p.Outcome())
	}

	// 3. the reconcile query keeps failing: still nothing moves, and it is asked on every run inside the window
	asked := s.calls(mock.OpGetPayment)
	s.clock.Advance(time.Hour)
	if out := s.run(t); out.QueryFailures != 1 || s.payment(t, orderID).Status() != payment.StatusCancelled {
		t.Fatalf("reconcile while down: %+v", out)
	}
	if s.calls(mock.OpGetPayment) != asked+1 {
		t.Fatalf("positive control: the provider was asked %d times since, want 1", s.calls(mock.OpGetPayment)-asked)
	}

	// 4. the provider is back inside the window: the money is found, captured and sent back
	down = false
	s.clock.Advance(time.Hour)
	s.run(t)
	if got := s.payment(t, orderID).Status(); got != payment.StatusRefunding {
		t.Fatalf("after recovery: %s, want REFUNDING", got)
	}
	ev := s.rec.events()
	if len(ev) != 2 || !strings.HasPrefix(ev[0], "PaymentCaptured:") || !strings.HasPrefix(ev[1], "PaymentRefundRequested:") {
		t.Fatalf("outbox = %v, want captured then refund requested", ev)
	}
}
