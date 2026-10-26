package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

// Integration test: a throwaway Postgres container, or TEST_DATABASE_URL (see pgtest).
func TestOrderRepository_Postgres(t *testing.T) {
	url := pgtest.URL(t)

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
	if v, dirty, err := platformpg.Version(c, cfg, m); err != nil || v != 6 || dirty {
		t.Fatalf("version = %d dirty=%v err=%v, want 6 clean", v, dirty, err)
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
	if got.Version() != 1 {
		t.Fatalf("version after insert = %d, want 1", got.Version())
	}

	// A fresh aggregate with a taken id is a duplicate, never an overwrite.
	dup, err := order.New(order.Params{ID: "o-1", CustomerID: "c-9", Items: []order.LineItem{li("Z", 1, 1)}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Save(c, dup); !errors.Is(err, order.ErrDuplicate) {
		t.Fatalf("fresh over existing: err = %v, want ErrDuplicate", err)
	}

	// Two writers load version 1 and both cancel: the first save wins, the
	// second is stale (under READ COMMITTED the real loser blocks on the row
	// lock, re-checks the WHERE and matches nothing — same outcome).
	other, err := repo.FindByID(c, "o-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range []*order.Order{got, other} {
		if err := x.Cancel(now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.Save(c, got); err != nil {
		t.Fatalf("Save (update): %v", err)
	}
	if err := repo.Save(c, other); !errors.Is(err, order.ErrStale) || !errors.Is(err, kernel.ErrConcurrentUpdate) {
		t.Fatalf("second writer: err = %v, want ErrStale wrapping kernel.ErrConcurrentUpdate", err)
	}
	var de *kernel.Error
	if errors.As(order.ErrStale, &de) {
		t.Fatal("ErrStale must not be a kernel.Error: the consumer would drop instead of retry")
	}
	reloaded, err := repo.FindByID(c, "o-1")
	if err != nil {
		t.Fatal(err)
	}
	// Update path: new status, items replaced (not appended), version bumped once.
	assertEqual(t, got, reloaded)
	if reloaded.Status() != order.StatusCancelled || reloaded.Version() != 2 {
		t.Fatalf("after update: status=%v version=%d, want CANCELLED 2", reloaded.Status(), reloaded.Version())
	}

	if _, err := repo.FindByID(c, "nope"); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("missing: err = %v, want ErrNotFound", err)
	}

	cancelled, cancel := context.WithCancel(c)
	cancel()
	if _, err := repo.FindByID(cancelled, "o-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx: err = %v", err)
	}

	// List: keyset paging in SQL. o-1 (cancelled, c-1) already exists; add
	// four more, two sharing a timestamp so the id tie-breaker runs in Postgres.
	for i, spec := range []struct {
		id, customer string
		at           time.Time
	}{
		{"o-2", "c-2", now.Add(2 * time.Second)},
		{"o-3", "c-1", now.Add(3 * time.Second)},
		{"o-4", "c-1", now.Add(3 * time.Second)},
		{"o-5", "c-2", now.Add(5 * time.Second)},
	} {
		o, err := order.New(order.Params{ID: order.ID(spec.id), CustomerID: spec.customer, Items: []order.LineItem{li("A", 1, 100)}, Now: spec.at})
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.Save(c, o); err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
	}

	var ids []string
	var after *order.Cursor
	for page := 0; ; page++ {
		got, next, err := repo.List(c, order.ListParams{Limit: 2, After: after})
		if err != nil {
			t.Fatalf("List page %d: %v", page, err)
		}
		for _, o := range got {
			ids = append(ids, o.ID().String())
			if len(o.Items()) == 0 {
				t.Fatalf("List: order %s came back without items", o.ID())
			}
		}
		if next == nil {
			break
		}
		after = next
	}
	if want := "[o-5 o-4 o-3 o-2 o-1]"; fmt.Sprint(ids) != want {
		t.Fatalf("List walk = %v, want %v", ids, want)
	}

	pending := order.StatusPending
	filtered, next, err := repo.List(c, order.ListParams{CustomerID: "c-1", Status: &pending, Limit: 10})
	if err != nil || next != nil || len(filtered) != 2 || filtered[0].ID() != "o-4" || filtered[1].ID() != "o-3" {
		t.Fatalf("List filtered = %v next=%v err=%v", filtered, next, err)
	}

	// ── count by status: one row per non-empty status, summing to every order ──
	all, _, err := repo.List(c, order.ListParams{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	counts, err := repo.CountByStatus(c)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	byStatus := map[order.Status]order.StatusCount{}
	for _, sc := range counts {
		if sc.Count() == 0 || sc.OldestUpdatedAt().IsZero() {
			t.Fatalf("empty status reported: %+v", sc)
		}
		total += sc.Count()
		byStatus[sc.Status()] = sc
	}
	if total != int64(len(all)) || byStatus[order.StatusPending].Count() < 2 {
		t.Fatalf("CountByStatus = %+v, want total %d with >= 2 PENDING", counts, len(all))
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
