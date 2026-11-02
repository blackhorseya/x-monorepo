package usecase_test

import (
	"context"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
)

// The sweep's lists are bounded (Batch), and a payment that is looked at without leaving the list (not
// paid, or the query failed) must not keep the front of the list for ever: the cursor (MarkSwept) lets the
// next one through. These specs fix that against the memory repository; the Postgres one has its own
// (infra/postgres/saga_races_test.go).

func (s *sweepEnv) sweepWith(provider port.PaymentProvider, batch int) *usecase.ExpireSweep {
	return usecase.NewExpireSweep(s.repo, s.webhooks, provider, s.rec, s.rec, memory.NewTransactor(), s.clock, dto.SweepConfig{
		ReconcileWindow: reconcileWindow, Interval: sweepInterval, Batch: batch, WebhookRetention: retention,
	})
}

func (s *sweepEnv) runWith(t *testing.T, sw *usecase.ExpireSweep) *dto.ExpireOutput {
	t.Helper()
	out, err := sw.Execute(context.Background(), dto.Ack{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	return out
}

// failingFor is a provider that cannot answer about one checkout and behaves as usual about the rest.
type failingFor struct {
	port.PaymentProvider
	ref string
}

func (f failingFor) GetPayment(c context.Context, ref string) (dto.ProviderPaymentOutput, error) {
	if ref == f.ref {
		return dto.ProviderPaymentOutput{}, port.ErrProviderUnavailable
	}

	return f.PaymentProvider.GetPayment(c, ref)
}

// More cancelled payments than one batch, the paid one last: every run looks at the one it looked at
// longest ago, so a healthy provider has been asked about all of them after as many runs as there are
// payments, and the paid one is found.
func TestSweep_ReconcileRotatesThroughMoreEligibleThanTheBatch(t *testing.T) {
	c := context.Background()
	s := newSweepEnv(t)
	orders := []string{"ord-a", "ord-b", "ord-c"}
	for _, o := range orders {
		s.seedPaid(t, o, true)
		if _, err := s.cancelled().Execute(c, dto.OrderEndedInput{OrderID: o}); err != nil {
			t.Fatal(err)
		}
		s.clock.Advance(time.Minute) // a distinct end time each, oldest first
	}
	last := s.payment(t, "ord-c")
	if _, err := s.prov.Pay(last.ProviderRef(), money(t)); err != nil {
		t.Fatal(err)
	}

	sw := s.sweepWith(s.prov, 1)
	for range orders {
		s.clock.Advance(sweepInterval)
		s.runWith(t, sw)
	}

	if got := s.calls(mock.OpGetPayment); got != len(orders) {
		t.Fatalf("%d provider queries in %d runs, want one per payment", got, len(orders))
	}
	if st := s.payment(t, "ord-c").Status(); st != payment.StatusRefunding {
		t.Fatalf("the paid cancellation behind two unpaid ones: status = %s, want REFUNDING", st)
	}
}

// The same for the due PENDING list: a payment whose query keeps failing stays PENDING (a failed query
// moves nothing), and must not keep the one behind it from being settled.
func TestSweep_AFailingQueryDoesNotHoldTheFrontOfTheDueList(t *testing.T) {
	s := newSweepEnv(t)
	s.seedPaid(t, "ord-a", true)
	s.clock.Advance(time.Minute)
	s.seedPaid(t, "ord-b", true)
	s.pastWindow()
	sw := s.sweepWith(failingFor{PaymentProvider: s.prov, ref: s.payment(t, "ord-a").ProviderRef()}, 1)

	s.runWith(t, sw)
	s.clock.Advance(sweepInterval)
	s.runWith(t, sw)

	if st := s.payment(t, "ord-b").Status(); st != payment.StatusExpired {
		t.Fatalf("ord-b behind a failing ord-a: status = %s, want EXPIRED", st)
	}
	if st := s.payment(t, "ord-a").Status(); st != payment.StatusPending {
		t.Fatalf("ord-a (could not be asked): status = %s, want it untouched PENDING", st)
	}
}

// What the operator sees: after each run, how many are owed a query and how long the least recently
// looked-at has waited; and an alert once that is more than half the window — the point from which the
// remaining runs can no longer be counted on to reach every payment.
func TestSweep_ReportsHowFarBehindTheReconcileIs(t *testing.T) {
	c := context.Background()
	setup := func(t *testing.T) *sweepEnv {
		s := newSweepEnv(t)
		for _, o := range []string{"ord-a", "ord-b"} {
			s.seedPaid(t, o, true)
			if _, err := s.cancelled().Execute(c, dto.OrderEndedInput{OrderID: o}); err != nil {
				t.Fatal(err)
			}
		}
		s.clock.Advance(13 * time.Hour) // past half of the 24h window, nothing asked yet

		return s
	}

	t.Run("a batch too small for the list: one left waiting 13h, alerted", func(t *testing.T) {
		s := setup(t)
		out := s.runWith(t, s.sweepWith(s.prov, 1))
		if out.ReconcileEligible != 2 || out.OldestUnchecked != 13*time.Hour {
			t.Fatalf("out = %+v, want 2 eligible and 13h", out)
		}
		if got := s.rec.backlogs; len(got) != 1 || got[0] != [2]time.Duration{2, 13 * time.Hour} {
			t.Fatalf("backlog reports = %v", got)
		}
		if got := s.rec.raised(); len(got) != 1 || got[0] != "reconcile_backlog:" {
			t.Fatalf("alerts = %v, want one reconcile_backlog", got)
		}
	})

	t.Run("a batch that covers the list: nobody waits, no alert", func(t *testing.T) {
		s := setup(t)
		out := s.runWith(t, s.sweepWith(s.prov, 100))
		if out.ReconcileEligible != 2 || out.OldestUnchecked != 0 {
			t.Fatalf("out = %+v, want 2 eligible and 0 waiting", out)
		}
		if got := s.rec.raised(); len(got) != 0 {
			t.Fatalf("alerts = %v, want none", got)
		}
	})
}
