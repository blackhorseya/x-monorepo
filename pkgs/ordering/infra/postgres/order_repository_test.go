package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"gorm.io/gorm"

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
	if v, dirty, err := platformpg.Version(c, cfg, m); err != nil || v != 8 || dirty {
		t.Fatalf("version = %d dirty=%v err=%v, want 8 clean", v, dirty, err)
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

// Idempotency keys: ON CONFLICT DO NOTHING on the partial unique index. Losing
// the race writes nothing and must leave the surrounding transaction usable;
// a taken id must still be a plain duplicate, not a swallowed conflict.
func TestOrderRepository_IdempotencyKey_Postgres(t *testing.T) {
	c := context.Background()
	url := pgtest.URL(t)
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
	if err := platformpg.Up(c, cfg, postgres.Migration()); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`TRUNCATE ordering.orders CASCADE`).Error; err != nil {
		t.Fatal(err)
	}

	repo := postgres.NewOrderRepository(db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	usd, _ := kernel.NewMoney(100, "USD")
	item, _ := order.NewLineItem("A", 1, usd)
	mk := func(id, customer, key string) *order.Order {
		o, err := order.New(order.Params{ID: order.ID(id), CustomerID: customer, Items: []order.LineItem{item}, Now: now, IdempotencyKey: key})
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	count := func(id string) (n int64) {
		if err := db.Raw(`SELECT (SELECT count(*) FROM ordering.orders WHERE id = ?) + (SELECT count(*) FROM ordering.order_items WHERE order_id = ?)`, id, id).Scan(&n).Error; err != nil {
			t.Fatal(err)
		}
		return n
	}

	if err := repo.Save(c, mk("o-1", "c-1", "k")); err != nil {
		t.Fatal(err)
	}
	if err := repo.Save(c, mk("o-2", "c-1", "k")); !errors.Is(err, order.ErrIdempotencyConflict) {
		t.Fatalf("same customer, same key: err = %v, want ErrIdempotencyConflict", err)
	}
	if n := count("o-2"); n != 0 {
		t.Fatalf("losing save left %d rows behind", n)
	}
	if err := repo.Save(c, mk("o-1", "c-1", "k-other")); !errors.Is(err, order.ErrDuplicate) {
		t.Fatalf("taken id with a fresh key: err = %v, want ErrDuplicate", err)
	}
	for _, o := range []*order.Order{mk("o-3", "c-2", "k"), mk("o-4", "c-1", ""), mk("o-5", "c-1", "")} {
		if err := repo.Save(c, o); err != nil {
			t.Fatalf("%s: %v", o.ID(), err)
		}
	}

	got, err := repo.FindByIdempotencyKey(c, "c-1", "k")
	if err != nil || got.ID() != "o-1" || got.IdempotencyKey() != "k" || got.RequestHash() != order.FingerprintV2([]order.RequestLine{mustLine(t, "A", 1)}) {
		t.Fatalf("lookup = %v, %v", got, err)
	}
	if _, err := repo.FindByIdempotencyKey(c, "c-9", "k"); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("other customer's key: err = %v, want ErrNotFound", err)
	}
	if _, err := repo.FindByIdempotencyKey(c, "c-1", ""); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("empty key must not match keyless rows: err = %v", err)
	}

	// Inside one transaction a lost race is not an abort: the same tx reads the winner back.
	err = platformpg.Transaction(c, db, func(c context.Context, _ *gorm.DB) error {
		if err := repo.Save(c, mk("o-6", "c-1", "k")); !errors.Is(err, order.ErrIdempotencyConflict) {
			return fmt.Errorf("save: %w", err)
		}
		w, err := repo.FindByIdempotencyKey(c, "c-1", "k")
		if err != nil || w.ID() != "o-1" {
			return fmt.Errorf("read back: %v, %w", w, err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("transaction after a lost race: %v", err)
	}

	// An update (cancel) neither drops nor changes the key.
	_ = got.Cancel(now.Add(time.Minute))
	if err := repo.Save(c, got); err != nil {
		t.Fatal(err)
	}
	if again, err := repo.FindByIdempotencyKey(c, "c-1", "k"); err != nil || again.Status() != order.StatusCancelled {
		t.Fatalf("after update: %v, %v", again, err)
	}
}

func mustLine(t *testing.T, sku string, qty int32) order.RequestLine {
	t.Helper()
	l, err := order.NewRequestLine(sku, qty)
	if err != nil {
		t.Fatal(err)
	}

	return l
}
