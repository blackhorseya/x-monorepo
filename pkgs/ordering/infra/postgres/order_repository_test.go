package postgres_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

// Integration test: needs TEST_DATABASE_URL (CI provides a Postgres service;
// locally e.g. postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable).
func TestOrderRepository_Postgres(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("TEST_DATABASE_URL must be set in CI: a skipped integration test would be a false green")
		}
		t.Skip("TEST_DATABASE_URL not set")
	}

	c := context.Background()
	cfg := config.Postgres{URL: url}
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	db, cleanup, err := platformpg.NewDB(cfg, tel)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	m := postgres.Migration()
	// Down → Up exercises the .down.sql files too; start from whatever state the DB is in.
	if err := platformpg.Up(c, cfg, m); err != nil {
		t.Fatal(err)
	}
	if err := platformpg.Down(c, cfg, m); err != nil {
		t.Fatal(err)
	}
	if err := platformpg.Up(c, cfg, m); err != nil {
		t.Fatal(err)
	}
	if v, dirty, err := platformpg.Version(c, cfg, m); err != nil || v != 1 || dirty {
		t.Fatalf("version = %d dirty=%v err=%v, want 1 clean", v, dirty, err)
	}

	// The version table must live inside the BC schema, never in public:
	// two BCs on one database would otherwise share one version number.
	var n int64
	if err := db.WithContext(c).Raw(
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = ? AND table_name = 'schema_migrations'`, postgres.Schema,
	).Scan(&n).Error; err != nil || n != 1 {
		t.Fatalf("schema_migrations in %q: count=%d err=%v", postgres.Schema, n, err)
	}
	if err := db.WithContext(c).Exec(`TRUNCATE ordering.orders CASCADE`).Error; err != nil {
		t.Fatal(err)
	}

	repo := postgres.NewOrderRepository(db)
	// Postgres keeps microseconds; Go keeps nanoseconds + monotonic clock.
	now := time.Now().UTC().Truncate(time.Microsecond)
	usd := func(n int64) kernel.Money { m, _ := kernel.NewMoney(n, "USD"); return m }
	li := func(sku string, qty int32, price int64) order.LineItem {
		it, err := order.NewLineItem(sku, qty, usd(price))
		if err != nil {
			t.Fatal(err)
		}
		return it
	}

	o, err := order.New(order.Params{ID: "o-1", CustomerID: "c-1", Items: []order.LineItem{li("B", 2, 1299), li("A", 1, 500)}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Save(c, o); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := repo.FindByID(c, "o-1")
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	assertEqual(t, o, got)

	// Update path: same id, new status, items replaced (not appended).
	if err := o.Cancel(now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := repo.Save(c, o); err != nil {
		t.Fatalf("Save (update): %v", err)
	}
	got, err = repo.FindByID(c, "o-1")
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, o, got)
	if got.Status() != order.StatusCancelled {
		t.Fatalf("status = %v", got.Status())
	}

	if _, err := repo.FindByID(c, "nope"); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("missing: err = %v, want ErrNotFound", err)
	}

	cancelled, cancel := context.WithCancel(c)
	cancel()
	if _, err := repo.FindByID(cancelled, "o-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx: err = %v", err)
	}
}

func assertEqual(t *testing.T, want, got *order.Order) {
	t.Helper()
	if got.ID() != want.ID() || got.CustomerID() != want.CustomerID() || got.Status() != want.Status() {
		t.Fatalf("header mismatch: got %v/%v/%v", got.ID(), got.CustomerID(), got.Status())
	}
	if !got.CreatedAt().Equal(want.CreatedAt()) || !got.UpdatedAt().Equal(want.UpdatedAt()) {
		t.Fatalf("timestamps: got %v/%v want %v/%v", got.CreatedAt(), got.UpdatedAt(), want.CreatedAt(), want.UpdatedAt())
	}
	if got.Total() != want.Total() {
		t.Fatalf("total: got %v want %v", got.Total(), want.Total())
	}
	wi, gi := want.Items(), got.Items()
	if len(wi) != len(gi) {
		t.Fatalf("items: got %d want %d", len(gi), len(wi))
	}
	for i := range wi {
		if wi[i] != gi[i] {
			t.Fatalf("item %d: got %+v want %+v (order must be preserved)", i, gi[i], wi[i])
		}
	}
}
