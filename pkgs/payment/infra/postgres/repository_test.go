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
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
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
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(telCleanup)
	db, closeDB, err := platformpg.NewDB(cfg, tel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeDB)
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
	checkoutEnds := t0.Add(10 * time.Minute)
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
