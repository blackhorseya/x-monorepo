package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/memory"
)

var t0 = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)

func newPayment(t *testing.T, id, order string) *payment.Payment {
	t.Helper()
	amount, err := kernel.NewMoney(1000, "TWD")
	if err != nil {
		t.Fatal(err)
	}
	p, err := payment.New(payment.NewParams{ID: payment.ID(id), OrderID: order, CustomerID: "cust", Total: amount, ExpiresAt: t0.Add(time.Hour)}, t0)
	if err != nil {
		t.Fatal(err)
	}

	return p
}

// The memory repository honours the contract of the Postgres one: version 1 on insert, +1 per save, the
// loaded copy is not the stored one, a lost race is ErrStale, a second payment for an order is refused.
func TestPaymentRepository_Contract(t *testing.T) {
	c := context.Background()
	repo := memory.NewPaymentRepository()

	if _, err := repo.FindByOrderID(c, "ord-1"); !errors.Is(err, payment.ErrNotFound) {
		t.Fatalf("missing order: %v", err)
	}
	p := newPayment(t, "pay-1", "ord-1")
	if err := repo.Save(c, p); err != nil {
		t.Fatal(err)
	}
	if err := repo.Save(c, newPayment(t, "pay-2", "ord-1")); !errors.Is(err, payment.ErrDuplicateForOrder) {
		t.Fatalf("second payment for the order: %v, want ErrDuplicateForOrder", err)
	}
	if err := repo.Save(c, newPayment(t, "pay-1", "ord-2")); !errors.Is(err, payment.ErrDuplicateForOrder) {
		t.Fatalf("reused payment id: %v, want ErrDuplicateForOrder", err)
	}

	a, err := repo.FindByOrderID(c, "ord-1")
	if err != nil || a.Version() != 1 || a.Status() != payment.StatusPending {
		t.Fatalf("loaded = %+v, %v; want version 1 PENDING", a, err)
	}
	b, _ := repo.FindByOrderID(c, "ord-1")

	a.OrderCancelled(t0.Add(time.Minute))
	if err = repo.Save(c, a); err != nil {
		t.Fatal(err)
	}
	// b loaded the same version: it lost
	b.OrderCompleted(t0.Add(time.Minute))
	if err = repo.Save(c, b); !errors.Is(err, payment.ErrStale) {
		t.Fatalf("save of a stale copy: %v, want ErrStale", err)
	}
	got, _ := repo.FindByOrderID(c, "ord-1")
	if got.Version() != 2 || got.Status() != payment.StatusCancelled || got.Outcome() != payment.OutcomeCancelled {
		t.Fatalf("stored = %+v, want version 2 CANCELLED by OrderCancelled", got)
	}
	// aliasing: mutating a loaded copy changes nothing stored
	got.Capture(payment.SourceWebhook, got.Amount(), t0)
	again, _ := repo.FindByOrderID(c, "ord-1")
	if again.Status() != payment.StatusCancelled {
		t.Fatalf("a loaded copy aliases the stored one: %s", again.Status())
	}
	// a version nobody stored
	ghost := newPayment(t, "pay-9", "ord-9")
	ghostLoaded, _ := payment.Reconstitute(payment.ReconstituteParams{ID: "pay-9", OrderID: "ord-9", CustomerID: "cust", Amount: ghost.Amount(), Status: payment.StatusPending, Outcome: payment.OutcomeNone, ExpiresAt: t0.Add(time.Hour), Version: 5})
	if err = repo.Save(c, ghostLoaded); !errors.Is(err, payment.ErrStale) {
		t.Fatalf("update of a payment that was never stored: %v, want ErrStale", err)
	}
	// a cancelled context is honoured
	cc, cancel := context.WithCancel(c)
	cancel()
	if _, err = repo.FindByOrderID(cc, "ord-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("find with a cancelled context: %v", err)
	}
	if err = repo.Save(cc, newPayment(t, "pay-3", "ord-3")); !errors.Is(err, context.Canceled) {
		t.Fatalf("save with a cancelled context: %v", err)
	}
}

func TestTransactor_RunsFn(t *testing.T) {
	ran := false
	if err := memory.NewTransactor().Do(context.Background(), func(context.Context) error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("ran=%v err=%v", ran, err)
	}
}
