package postgres_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

var t0 = time.Date(2026, 11, 2, 0, 0, 0, 123456000, time.UTC)

func twd(t *testing.T, n int64) kernel.Money {
	t.Helper()
	m, err := kernel.NewMoney(n, "TWD")
	if err != nil {
		t.Fatal(err)
	}

	return m
}

// testDB is a migrated, empty payment schema. Cleanup (registered after the pool's own, so it runs
// BEFORE the pool closes) empties it again and fails the test if that fails: a dirty table must not
// reach the next test or the next -count round.
func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	cfg := config.Postgres{URL: pgtest.URL(t)}
	tel := pgtest.Telemetry(t)
	db := pgtest.DB(t, cfg, tel)
	if err := platformpg.Up(context.Background(), cfg, postgres.Migration()); err != nil {
		t.Fatal(err)
	}
	truncate := func() {
		if err := db.Exec("TRUNCATE payment.payments").Error; err != nil {
			t.Errorf("truncate payment.payments: %v", err)
		}
	}
	truncate()
	t.Cleanup(truncate)

	return db
}

func newPayment(t *testing.T, id, order string) *payment.Payment {
	t.Helper()
	p, err := payment.New(payment.NewParams{ID: payment.ID(id), OrderID: order, CustomerID: "cust", Total: twd(t, 1000), ExpiresAt: t0.Add(30 * time.Minute)}, t0)
	if err != nil {
		t.Fatal(err)
	}

	return p
}

// Down → Up exercises the .down.sql too; the schema is at version 1.
func TestMigration_UpDownUp(t *testing.T) {
	cfg := config.Postgres{URL: pgtest.URL(t)}
	c := context.Background()
	m := postgres.Migration()
	for _, step := range []func(context.Context, config.Postgres, platformpg.Migration) error{platformpg.Up, platformpg.Down, platformpg.Up} {
		if err := step(c, cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	if v, dirty, err := platformpg.Version(c, cfg, m); err != nil || v != 2 || dirty {
		t.Fatalf("version = %d dirty=%v err=%v, want 2 clean", v, dirty, err)
	}
}

// The repository on a real Postgres: every field survives the round trip (UTC, microseconds, the
// nullable times), insert is version 1, an update bumps it, a copy loaded before someone else's save
// loses with ErrStale, and a second payment for an order is ErrDuplicateForOrder.
func TestPaymentRepository_Postgres(t *testing.T) {
	db := testDB(t)
	c := context.Background()
	repo := postgres.NewPaymentRepository(db)

	if _, err := repo.FindByOrderID(c, "ord-1"); !errors.Is(err, payment.ErrNotFound) {
		t.Fatalf("missing order: %v", err)
	}

	p := newPayment(t, "pay-1", "ord-1")
	if err := repo.Save(c, p); err != nil {
		t.Fatal(err)
	}
	got, err := repo.FindByOrderID(c, "ord-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID() != "pay-1" || got.OrderID() != "ord-1" || got.CustomerID() != "cust" || got.Amount() != twd(t, 1000) ||
		got.Status() != payment.StatusPending || got.Outcome() != payment.OutcomeNone || got.Version() != 1 ||
		!got.ExpiresAt().Equal(t0.Add(30*time.Minute)) || !got.UpdatedAt().Equal(t0) ||
		got.CheckoutURL() != "" || !got.CheckoutExpiresAt().IsZero() || !got.TerminalAt().IsZero() || got.ProviderRef() != "" {
		t.Fatalf("round trip lost something: %+v", got)
	}

	if err = repo.Save(c, newPayment(t, "pay-2", "ord-1")); !errors.Is(err, payment.ErrDuplicateForOrder) {
		t.Fatalf("second payment for the order: %v, want ErrDuplicateForOrder", err)
	}
	if err = repo.Save(c, newPayment(t, "pay-1", "ord-2")); !errors.Is(err, payment.ErrDuplicateForOrder) {
		t.Fatalf("reused payment id: %v, want ErrDuplicateForOrder", err)
	}

	// update with a checkout (nullable time set)
	stale, _ := repo.FindByOrderID(c, "ord-1")
	checkoutEnds := t0.Add(30 * time.Minute)
	if err = got.AttachCheckout("mock", "ref-1", "https://pay.example/c/1", checkoutEnds, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = repo.Save(c, got); err != nil {
		t.Fatal(err)
	}
	if err = repo.Save(c, got); !errors.Is(err, payment.ErrStale) {
		t.Fatalf("saving the same loaded copy twice: %v, want ErrStale", err)
	}
	stale.OrderCancelled(t0.Add(2 * time.Second))
	if err = repo.Save(c, stale); !errors.Is(err, payment.ErrStale) {
		t.Fatalf("a copy loaded before someone else's save: %v, want ErrStale", err)
	}

	again, _ := repo.FindByOrderID(c, "ord-1")
	if again.Version() != 2 || again.Provider() != "mock" || again.ProviderRef() != "ref-1" || again.CheckoutURL() != "https://pay.example/c/1" ||
		!again.CheckoutExpiresAt().Equal(checkoutEnds) || again.Status() != payment.StatusPending {
		t.Fatalf("after the update: %+v", again)
	}

	// into a terminal state with the terminal time set, then back out of the nullable column's way
	again.OrderCancelled(t0.Add(time.Minute))
	if err = repo.Save(c, again); err != nil {
		t.Fatal(err)
	}
	end, _ := repo.FindByOrderID(c, "ord-1")
	if end.Status() != payment.StatusCancelled || end.Outcome() != payment.OutcomeCancelled || !end.TerminalAt().Equal(t0.Add(time.Minute)) || end.Version() != 3 {
		t.Fatalf("cancelled: %+v", end)
	}

	cc, cancel := context.WithCancel(c)
	cancel()
	if _, err = repo.FindByOrderID(cc, "ord-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("find with a cancelled context: %v", err)
	}
	if err = repo.Save(cc, newPayment(t, "pay-9", "ord-9")); !errors.Is(err, context.Canceled) {
		t.Fatalf("save with a cancelled context: %v", err)
	}
}

// A row the schema accepted is a row Reconstitute accepts: a row that breaks an invariant is a load
// error, not a payment in an impossible state. (The schema's own refusals are in schema_test.go.)
func TestPaymentRepository_RefusesACorruptRow(t *testing.T) {
	db := testDB(t)
	c := context.Background()
	// Bypass the check constraint the way a manual repair could: with the constraint off for the session.
	if err := db.Exec(`ALTER TABLE payment.payments DROP CONSTRAINT payments_outcome_fits_status`).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// restore the constraint for the next test: the table is empty again by then
		if err := db.Exec(`TRUNCATE payment.payments`).Error; err != nil {
			t.Errorf("truncate: %v", err)
		}
		if err := db.Exec(`ALTER TABLE payment.payments ADD CONSTRAINT payments_outcome_fits_status CHECK (
			(status IN ('PENDING', 'CAPTURED') AND order_outcome = 'NONE')
			OR (status IN ('CANCELLED', 'REFUNDING', 'REFUNDED') AND order_outcome <> 'NONE')
			OR status IN ('EXPIRED', 'NEEDS_REVIEW'))`).Error; err != nil {
			t.Errorf("restore constraint: %v", err)
		}
	})
	if err := db.Exec(`INSERT INTO payment.payments (id, order_id, customer_id, amount, currency, status, order_outcome, expires_at, version, updated_at)
		VALUES ('pay-bad', 'ord-bad', 'cust', 10, 'TWD', 'CANCELLED', 'NONE', now(), 1, now())`).Error; err != nil {
		t.Fatal(err)
	}
	repo := postgres.NewPaymentRepository(db)
	if _, err := repo.FindByOrderID(c, "ord-bad"); !errors.Is(err, payment.ErrInvalidOutcome) {
		t.Fatalf("a CANCELLED payment that never heard the order end loaded as: %v, want ErrInvalidOutcome", err)
	}
}

func TestPaymentRepository_FindersAndOutcomes_Postgres(t *testing.T) {
	db := testDB(t)
	c := context.Background()
	repo := postgres.NewPaymentRepository(db)
	p := newPayment(t, "pay-1", "ord-1")
	if err := p.AttachCheckout("mock", "ref-1", "https://pay.example/c/1", t0.Add(30*time.Minute), t0); err != nil {
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

	out := postgres.NewOutcomeRepository(db)
	if err := db.Exec("TRUNCATE payment.order_outcomes").Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Exec("TRUNCATE payment.order_outcomes").Error; err != nil {
			t.Errorf("truncate order_outcomes: %v", err)
		}
	})
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
	// the table refuses an outcome that is not one the domain records
	if err = db.Exec(`INSERT INTO payment.order_outcomes (order_id, outcome, recorded_at) VALUES ('ord-9', 'NONE', now())`).Error; err == nil {
		t.Fatal("the schema accepted outcome NONE")
	}
}

func aWebhook(t *testing.T, id, typ string, amount kernel.Money) *payment.WebhookEvent {
	t.Helper()
	w, err := payment.NewWebhookEvent("mock", id, typ, "ref-1", amount, []byte(`{"id":"`+id+`"}`), t0)
	if err != nil {
		t.Fatal(err)
	}

	return w
}

// The webhook log on a real Postgres: the round trip (raw body, amount and currency, nullable processed_at),
// the dedup key, the processed mark and the retention delete with its sentinels.
func TestWebhookRepository_Postgres(t *testing.T) {
	db := testDB(t)
	c := context.Background()
	repo := postgres.NewWebhookRepository(db)
	if err := db.Exec("TRUNCATE payment.webhook_events").Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Exec("TRUNCATE payment.webhook_events").Error; err != nil {
			t.Errorf("truncate webhook_events: %v", err)
		}
	})

	if inserted, err := repo.Insert(c, aWebhook(t, "evt-1", payment.WebhookCaptured, twd(t, 1000))); err != nil || !inserted {
		t.Fatalf("insert = %v, %v", inserted, err)
	}
	if inserted, err := repo.Insert(c, aWebhook(t, "evt-1", "other.type", twd(t, 5))); err != nil || inserted {
		t.Fatalf("redelivery inserted again: %v, %v", inserted, err)
	}
	if inserted, err := repo.Insert(c, aWebhook(t, "evt-none", "payment.refunded", kernel.Money{})); err != nil || !inserted {
		t.Fatalf("insert without an amount = %v, %v", inserted, err)
	}

	got, err := repo.Find(c, "mock", "evt-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.EventType() != payment.WebhookCaptured || got.Ref() != "ref-1" || got.Amount() != twd(t, 1000) || string(got.Body()) != `{"id":"evt-1"}` ||
		!got.ReceivedAt().Equal(t0) || got.Processed() {
		t.Fatalf("round trip lost something: %+v", got)
	}
	noAmount, err := repo.Find(c, "mock", "evt-none")
	if err != nil || noAmount.Amount() != (kernel.Money{}) {
		t.Fatalf("an event without an amount: %+v %v", noAmount, err)
	}
	if _, err = repo.Find(c, "mock", "nope"); !errors.Is(err, payment.ErrNotFound) {
		t.Fatalf("Find unknown: %v", err)
	}

	for _, id := range []string{"old-1", "old-2", "old-3", "recent", "unprocessed"} {
		if _, err = repo.Insert(c, aWebhook(t, id, payment.WebhookCaptured, twd(t, 1))); err != nil {
			t.Fatal(err)
		}
	}
	// a payment under review, whose old processed notification must survive retention
	rev := newPayment(t, "pay-review", "ord-review")
	if err = rev.AttachCheckout("mock", "ref-review", "https://pay.example/c/r", t0.Add(30*time.Minute), t0); err != nil {
		t.Fatal(err)
	}
	rev.Capture(payment.SourceWebhook, twd(t, 1), t0) // does not match: NEEDS_REVIEW
	if err = postgres.NewPaymentRepository(db).Save(c, rev); err != nil {
		t.Fatal(err)
	}
	if rev.Status() != payment.StatusNeedsReview {
		t.Fatalf("fixture status = %s", rev.Status())
	}
	reviewed, err := payment.NewWebhookEvent("mock", "reviewed", payment.WebhookCaptured, "ref-review", twd(t, 1), []byte("{}"), t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.Insert(c, reviewed); err != nil {
		t.Fatal(err)
	}
	if err = repo.MarkProcessed(c, "mock", "reviewed", t0.Add(-200*time.Hour)); err != nil {
		t.Fatal(err)
	}

	long := t0.Add(-100 * time.Hour)
	for i, id := range []string{"old-1", "old-2", "old-3"} {
		if err = repo.MarkProcessed(c, "mock", id, long.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if err = repo.MarkProcessed(c, "mock", "old-1", t0); err != nil { // the first mark stands
		t.Fatal(err)
	}
	if err = repo.MarkProcessed(c, "mock", "recent", t0); err != nil {
		t.Fatal(err)
	}
	if got, _ = repo.Find(c, "mock", "old-1"); !got.ProcessedAt().Equal(long) {
		t.Fatalf("processed_at = %v, want the first mark %v", got.ProcessedAt(), long)
	}

	cutoff := t0.Add(-24 * time.Hour)
	n, err := repo.DeleteProcessedBefore(c, cutoff, 2)
	if err != nil || n != 2 {
		t.Fatalf("delete with limit 2 = %d, %v", n, err)
	}
	if _, err = repo.Find(c, "mock", "old-1"); !errors.Is(err, payment.ErrNotFound) {
		t.Fatalf("the oldest was not deleted first: %v", err)
	}
	if _, err = repo.Find(c, "mock", "old-3"); err != nil {
		t.Fatalf("a row beyond the limit was deleted: %v", err)
	}
	if n, err = repo.DeleteProcessedBefore(c, cutoff, 100); err != nil || n != 1 {
		t.Fatalf("second delete = %d, %v", n, err)
	}
	for _, id := range []string{"recent", "unprocessed", "evt-1", "evt-none", "reviewed"} {
		if _, err = repo.Find(c, "mock", id); err != nil {
			t.Fatalf("sentinel %s was deleted: %v", id, err)
		}
	}

	cc, cancel := context.WithCancel(c)
	cancel()
	for name, err := range map[string]error{
		"insert": func() error { _, err := repo.Insert(cc, aWebhook(t, "x", "t", kernel.Money{})); return err }(),
		"find":   func() error { _, err := repo.Find(cc, "mock", "x"); return err }(),
		"mark":   repo.MarkProcessed(cc, "mock", "x", t0),
		"delete": func() error { _, err := repo.DeleteProcessedBefore(cc, t0, 1); return err }(),
	} {
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%s with a cancelled context: %v", name, err)
		}
	}
}

// The schema refuses a notification the domain would not build.
func TestSchema_WebhookEvents(t *testing.T) {
	db := testDB(t)
	if err := db.Exec("TRUNCATE payment.webhook_events").Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Exec("TRUNCATE payment.webhook_events").Error; err != nil {
			t.Errorf("truncate webhook_events: %v", err)
		}
	})
	const q = `INSERT INTO payment.webhook_events (provider, provider_event_id, event_type, provider_ref, amount, currency, body, received_at) VALUES (?, ?, ?, '', ?, ?, ?, now())`
	if err := db.Exec(q, "mock", "ok", "t", 5, "TWD", []byte("x")).Error; err != nil {
		t.Fatalf("a valid row: %v", err)
	}
	if err := db.Exec(q, "mock", "ok2", "t", nil, nil, []byte("x")).Error; err != nil {
		t.Fatalf("a row without an amount: %v", err)
	}
	for name, args := range map[string][]any{
		"same key twice":          {"mock", "ok", "t", 5, "TWD", []byte("x")},
		"amount without currency": {"mock", "a1", "t", 5, nil, []byte("x")},
		"currency without amount": {"mock", "a2", "t", nil, "TWD", []byte("x")},
		"negative amount":         {"mock", "a3", "t", -1, "TWD", []byte("x")},
		"lower-case currency":     {"mock", "a4", "t", 1, "twd", []byte("x")},
		"empty provider":          {"", "a5", "t", 1, "TWD", []byte("x")},
		"empty event id":          {"mock", "", "t", 1, "TWD", []byte("x")},
		"body over 1 MiB":         {"mock", "a6", "t", 1, "TWD", make([]byte, 1<<20+1)},
	} {
		if err := db.Exec(q, args...).Error; err == nil {
			t.Errorf("%s: the schema accepted the row", name)
		}
	}
}

// ListPendingDue / ListReconcilable: the sweep's two lists, with sentinels on both sides of every boundary.
func TestPaymentRepository_Lists_Postgres(t *testing.T) {
	c := context.Background()
	db := testDB(t)
	repo := postgres.NewPaymentRepository(db)
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
		if err := p.AttachCheckout("mock", "ref-"+p.OrderID(), "https://pay.example/c", t0.Add(30*time.Minute), t0); err != nil {
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
	save("review", "ord-review", func(p *payment.Payment) { checkout(p); p.Capture(payment.SourceWebhook, twd(t, 1), t0) })

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
