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

func TestPaymentRepository_FindersAndOutcomes(t *testing.T) {
	c := context.Background()
	repo := memory.NewPaymentRepository()
	p := newPayment(t, "pay-1", "ord-1")
	if err := p.AttachCheckout("mock", "ref-1", "https://pay.example/c/1", t0.Add(10*time.Minute), t0); err != nil {
		t.Fatal(err)
	}
	if err := repo.Save(c, p); err != nil {
		t.Fatal(err)
	}

	byID, err := repo.FindByID(c, "pay-1")
	if err != nil || byID.OrderID() != "ord-1" {
		t.Fatalf("FindByID = %v, %v", byID, err)
	}
	byRef, err := repo.FindByProviderRef(c, "mock", "ref-1")
	if err != nil || byRef.ID() != "pay-1" {
		t.Fatalf("FindByProviderRef = %v, %v", byRef, err)
	}
	for name, call := range map[string]func() error{
		"unknown id":           func() error { _, err := repo.FindByID(c, "nope"); return err },
		"unknown ref":          func() error { _, err := repo.FindByProviderRef(c, "mock", "nope"); return err },
		"same ref, other name": func() error { _, err := repo.FindByProviderRef(c, "other", "ref-1"); return err },
		"empty ref":            func() error { _, err := repo.FindByProviderRef(c, "mock", ""); return err },
	} {
		if err := call(); !errors.Is(err, payment.ErrNotFound) {
			t.Fatalf("%s: %v, want ErrNotFound", name, err)
		}
	}
	cc, cancel := context.WithCancel(c)
	cancel()
	if _, err = repo.FindByID(cc, "pay-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("FindByID cancelled: %v", err)
	}
	if _, err = repo.FindByProviderRef(cc, "mock", "ref-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("FindByProviderRef cancelled: %v", err)
	}

	out := memory.NewOutcomeRepository()
	if got, err := out.Find(c, "ord-1"); err != nil || got != payment.OutcomeNone {
		t.Fatalf("nothing recorded: %v %v", got, err)
	}
	if err = out.Record(c, "ord-1", payment.OutcomeCancelled, t0); err != nil {
		t.Fatal(err)
	}
	if err = out.Record(c, "ord-1", payment.OutcomeCompletedUnpaid, t0); err != nil {
		t.Fatal(err)
	}
	if got, _ := out.Find(c, "ord-1"); got != payment.OutcomeCancelled {
		t.Fatalf("outcome = %s, want the first (CANCELLED)", got)
	}
	if _, err = out.Find(cc, "ord-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Find cancelled: %v", err)
	}
	if err = out.Record(cc, "ord-2", payment.OutcomeCancelled, t0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Record cancelled: %v", err)
	}
}

func aWebhook(t *testing.T, id, typ string) *payment.WebhookEvent {
	t.Helper()
	amount, _ := kernel.NewMoney(1000, "TWD")
	w, err := payment.NewWebhookEvent("mock", id, typ, "ref-1", amount, []byte(`{"id":"`+id+`"}`), t0)
	if err != nil {
		t.Fatal(err)
	}

	return w
}

// The webhook log: (provider, event id) deduplicates, processing is marked once, and the retention delete
// takes only PROCESSED rows older than the cutoff, oldest first, up to the limit — the sentinels (an
// unprocessed old row, a processed recent row) must survive, or "nothing was deleted" proves nothing.
func TestWebhookRepository(t *testing.T) {
	c := context.Background()
	reviewed := map[string]bool{"ref-review": true}
	repo := memory.NewWebhookRepository(func(provider, ref string) bool { return provider == "mock" && reviewed[ref] })
	review, err := payment.NewWebhookEvent("mock", "reviewed", payment.WebhookCaptured, "ref-review", aWebhook(t, "x", "t").Amount(), []byte("{}"), t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.Insert(c, review); err != nil {
		t.Fatal(err)
	}
	if err = repo.MarkProcessed(c, "mock", "reviewed", t0.Add(-100*time.Hour)); err != nil { // old and processed, but its payment is under review
		t.Fatal(err)
	}
	for _, id := range []string{"old-1", "old-2", "old-3", "recent", "unprocessed"} {
		if inserted, err := repo.Insert(c, aWebhook(t, id, payment.WebhookCaptured)); err != nil || !inserted {
			t.Fatalf("insert %s = %v, %v", id, inserted, err)
		}
	}
	if inserted, err := repo.Insert(c, aWebhook(t, "old-1", "something.else")); err != nil || inserted {
		t.Fatalf("a redelivery inserted again: %v, %v", inserted, err)
	}
	got, err := repo.Find(c, "mock", "old-1")
	if err != nil || got.EventType() != payment.WebhookCaptured || got.Processed() {
		t.Fatalf("Find = %+v, %v; want the first one stored, unprocessed", got, err)
	}
	if _, err = repo.Find(c, "mock", "nope"); !errors.Is(err, payment.ErrNotFound) {
		t.Fatalf("Find unknown: %v", err)
	}
	if _, err = repo.Find(c, "other", "old-1"); !errors.Is(err, payment.ErrNotFound) {
		t.Fatalf("Find under another provider: %v", err)
	}

	long := t0.Add(-100 * time.Hour)
	for _, id := range []string{"old-1", "old-2", "old-3"} {
		if err = repo.MarkProcessed(c, "mock", id, long); err != nil {
			t.Fatal(err)
		}
	}
	if err = repo.MarkProcessed(c, "mock", "old-1", t0); err != nil { // the first mark stands
		t.Fatal(err)
	}
	if err = repo.MarkProcessed(c, "mock", "recent", t0); err != nil {
		t.Fatal(err)
	}
	if err = repo.MarkProcessed(c, "mock", "ghost", t0); err != nil {
		t.Fatalf("marking an unknown event: %v", err)
	}
	if got, _ = repo.Find(c, "mock", "old-1"); !got.ProcessedAt().Equal(long) {
		t.Fatalf("processed_at = %v, want the first mark %v", got.ProcessedAt(), long)
	}

	cutoff := t0.Add(-24 * time.Hour)
	n, err := repo.DeleteProcessedBefore(c, cutoff, 2)
	if err != nil || n != 2 {
		t.Fatalf("delete with limit 2 = %d, %v; want 2", n, err)
	}
	if _, err = repo.Find(c, "mock", "old-1"); !errors.Is(err, payment.ErrNotFound) {
		t.Fatalf("the oldest was not the first deleted: %v", err)
	}
	if _, err = repo.Find(c, "mock", "old-3"); err != nil {
		t.Fatalf("a row beyond the limit was deleted: %v", err)
	}
	if n, err = repo.DeleteProcessedBefore(c, cutoff, 100); err != nil || n != 1 {
		t.Fatalf("second delete = %d, %v; want the remaining old one", n, err)
	}
	for _, id := range []string{"recent", "unprocessed", "reviewed"} {
		if _, err = repo.Find(c, "mock", id); err != nil {
			t.Fatalf("sentinel %s was deleted: %v", id, err)
		}
	}
	// a replay of a deleted event is "new" again: that is what retention costs, and why it must outlast the redelivery window
	if inserted, _ := repo.Insert(c, aWebhook(t, "old-1", payment.WebhookCaptured)); !inserted {
		t.Fatal("after retention, a redelivery is treated as new")
	}

	cc, cancel := context.WithCancel(c)
	cancel()
	for name, err := range map[string]error{
		"insert": func() error { _, err := repo.Insert(cc, aWebhook(t, "x", "t")); return err }(),
		"find":   func() error { _, err := repo.Find(cc, "mock", "x"); return err }(),
		"mark":   repo.MarkProcessed(cc, "mock", "x", t0),
		"delete": func() error { _, err := repo.DeleteProcessedBefore(cc, t0, 1); return err }(),
	} {
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%s with a cancelled context: %v", name, err)
		}
	}
}

// ListPendingDue / ListReconcilable: the sweep's two lists, with sentinels on both sides of every boundary.
func TestPaymentRepository_Lists(t *testing.T) {
	c := context.Background()
	repo := memory.NewPaymentRepository()
	exp := newPayment(t, "probe", "probe").ExpiresAt() // when the fixtures' window closes
	now := exp.Add(time.Hour)
	save := func(id, order string, mutate func(*payment.Payment)) {
		t.Helper()
		p := newPayment(t, id, order)
		if mutate != nil {
			mutate(p)
		}
		if err := repo.Save(c, p); err != nil {
			t.Fatal(err)
		}
	}
	checkout := func(p *payment.Payment) {
		if err := p.AttachCheckout("mock", "ref-"+p.OrderID(), "https://pay.example/c", t0.Add(10*time.Minute), t0); err != nil {
			t.Fatal(err)
		}
	}
	// pending: expires_at = t0+30m for all (newPayment). Make two due at different times and one not due.
	save("p-late", "ord-late", func(p *payment.Payment) {}) // due since t0+30m
	save("p-notdue", "ord-notdue", nil)
	save("p-captured", "ord-captured", func(p *payment.Payment) { p.Capture(payment.SourceWebhook, p.Amount(), t0) })

	due, err := repo.ListPendingDue(c, now, 10)
	if err != nil || len(due) != 2 {
		t.Fatalf("due at %v = %d, %v; want the two PENDING ones past their window", now, len(due), err)
	}
	if none, _ := repo.ListPendingDue(c, exp.Add(-time.Minute), 10); len(none) != 0 {
		t.Fatalf("%d payments due before any window closed", len(none))
	}
	if exact, _ := repo.ListPendingDue(c, exp, 10); len(exact) != 2 {
		t.Fatalf("at the exact moment the window closes %d are due, want 2", len(exact))
	}
	if limited, _ := repo.ListPendingDue(c, now, 1); len(limited) != 1 {
		t.Fatalf("limit 1 returned %d", len(limited))
	}

	// reconcilable: EXPIRED / CANCELLED with a checkout, ended at or after notBefore
	end := exp.Add(10 * time.Hour)
	save("e-ref", "ord-e-ref", func(p *payment.Payment) { checkout(p); p.Unpaid(end) })
	save("e-noref", "ord-e-noref", func(p *payment.Payment) { p.Unpaid(end) })
	save("c-ref", "ord-c-ref", func(p *payment.Payment) { checkout(p); p.OrderCancelled(end.Add(time.Minute)) })
	save("e-old", "ord-e-old", func(p *payment.Payment) { checkout(p); p.Unpaid(exp.Add(time.Minute)) })
	save("review", "ord-review", func(p *payment.Payment) { checkout(p); p.Capture(payment.SourceWebhook, small(t), t0) })

	got, err := repo.ListReconcilable(c, end.Add(-time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[payment.ID]bool{}
	for _, p := range got {
		ids[p.ID()] = true
	}
	if len(got) != 2 || !ids["e-ref"] || !ids["c-ref"] {
		t.Fatalf("reconcilable = %v, want exactly e-ref and c-ref (not: no checkout, ended before the cutoff, under review, pending)", ids)
	}
	if got[0].ID() != "e-ref" {
		t.Fatalf("first = %s, want the one whose window closes soonest (e-ref)", got[0].ID())
	}
	if all, _ := repo.ListReconcilable(c, exp, 10); len(all) != 3 {
		t.Fatalf("with an earlier cutoff %d are reconcilable, want 3 (e-old joins)", len(all))
	}
	if limited, _ := repo.ListReconcilable(c, exp, 2); len(limited) != 2 {
		t.Fatalf("limit 2 returned %d", len(limited))
	}

	cc, cancel := context.WithCancel(c)
	cancel()
	if _, err = repo.ListPendingDue(cc, now, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListPendingDue cancelled: %v", err)
	}
	if _, err = repo.ListReconcilable(cc, now, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListReconcilable cancelled: %v", err)
	}
}

func small(t *testing.T) kernel.Money {
	t.Helper()
	m, err := kernel.NewMoney(1, "TWD")
	if err != nil {
		t.Fatal(err)
	}

	return m
}
