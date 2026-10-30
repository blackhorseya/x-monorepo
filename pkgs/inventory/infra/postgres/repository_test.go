package postgres_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/stock"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

// Integration test: a throwaway Postgres container, or TEST_DATABASE_URL (see pgtest).
func TestRepositories_Postgres(t *testing.T) {
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
	for _, step := range []func(context.Context, config.Postgres, platformpg.Migration) error{platformpg.Up, platformpg.Down, platformpg.Up} {
		if err := step(c, cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	if v, dirty, err := platformpg.Version(c, cfg, m); err != nil || v != 9 || dirty {
		t.Fatalf("version = %d dirty=%v err=%v, want 9 clean", v, dirty, err)
	}
	for _, tbl := range []string{"inventory.stocks", "inventory.reservations"} {
		if err := db.WithContext(c).Exec("TRUNCATE " + tbl + " CASCADE").Error; err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	stocks := postgres.NewStockRepository(db)
	reservations := postgres.NewReservationRepository(db)

	// ── stock: insert, reload, optimistic lock ─────────────────────────────
	if _, err := stocks.FindBySKU(c, "A"); !errors.Is(err, stock.ErrNotFound) {
		t.Fatalf("missing sku: %v", err)
	}
	a, _ := stock.New("A", 5, now)
	if err := stocks.Save(c, a); err != nil {
		t.Fatal(err)
	}
	// Two writers both missed FindBySKU and created the SKU: the second insert
	// must lose, not overwrite (the gap an upsert-with-WHERE leaves at version 0).
	twin, _ := stock.New("A", 42, now)
	if err := stocks.Save(c, twin); !errors.Is(err, stock.ErrStale) {
		t.Fatalf("second fresh insert: err = %v, want ErrStale", err)
	}
	loaded, err := stocks.FindBySKU(c, "A")
	if err != nil || loaded.OnHand() != 5 || loaded.Version() != 1 || !loaded.UpdatedAt().Equal(now) {
		t.Fatalf("loaded = %+v, %v", loaded, err)
	}
	// Two readers of version 1: the first write wins, the second is stale.
	other, _ := stocks.FindBySKU(c, "A")
	if err := loaded.Restock(6, now); err != nil {
		t.Fatal(err)
	}
	if err := stocks.Save(c, loaded); err != nil {
		t.Fatal(err)
	}
	if err := other.Restock(7, now); err != nil {
		t.Fatal(err)
	}
	if err := stocks.Save(c, other); !errors.Is(err, stock.ErrStale) {
		t.Fatalf("stale write: %v", err)
	}
	reloaded, _ := stocks.FindBySKU(c, "A")
	if reloaded.OnHand() != 6 || reloaded.Version() != 2 {
		t.Fatalf("after race: on_hand=%d version=%d", reloaded.OnHand(), reloaded.Version())
	}
	// A fresh aggregate for an existing SKU (lost FindBySKU race) is stale too, not an overwrite.
	fresh, _ := stock.New("A", 99, now)
	if err := stocks.Save(c, fresh); !errors.Is(err, stock.ErrStale) {
		t.Fatalf("fresh over existing: %v", err)
	}

	// ── stock: keyset listing ascending by SKU ─────────────────────────────
	for _, sku := range []string{"C", "B"} {
		s, _ := stock.New(stock.SKU(sku), 1, now)
		if err := stocks.Save(c, s); err != nil {
			t.Fatal(err)
		}
	}
	page, next, err := stocks.List(c, stock.ListParams{Limit: 2})
	if err != nil || len(page) != 2 || page[0].SKU() != "A" || page[1].SKU() != "B" || next != "B" {
		t.Fatalf("list page 1 = %v next=%q err=%v", page, next, err)
	}
	page, next, err = stocks.List(c, stock.ListParams{Limit: 2, After: next})
	if err != nil || len(page) != 1 || page[0].SKU() != "C" || page[0].Version() != 1 || next != "" {
		t.Fatalf("list page 2 = %v next=%q err=%v", page, next, err)
	}
	// Filters: prefix is a literal (LIKE wildcards escaped), reserved > 0.
	for _, sku := range []string{"B_1", "B-1"} {
		s, _ := stock.New(stock.SKU(sku), 2, now)
		if sku == "B-1" {
			if short := stock.ReserveAll(map[string]*stock.Stock{sku: s}, []string{sku}, map[string]int32{sku: 1}, now); short != nil {
				t.Fatalf("reserve: short %v", short)
			}
		}
		if err := stocks.Save(c, s); err != nil {
			t.Fatal(err)
		}
	}
	if page, _, err := stocks.List(c, stock.ListParams{Limit: 10, SKUPrefix: "B_"}); err != nil || len(page) != 1 || page[0].SKU() != "B_1" {
		t.Fatalf("prefix with _ = %v err=%v (must not match B-1)", page, err)
	}
	if page, next, err := stocks.List(c, stock.ListParams{Limit: 1, SKUPrefix: "B"}); err != nil || len(page) != 1 || page[0].SKU() != "B" || next != "B" {
		t.Fatalf("prefix page 1 = %v next=%q err=%v", page, next, err)
	}
	if page, _, err := stocks.List(c, stock.ListParams{Limit: 10, OnlyReserved: true}); err != nil || len(page) != 1 || page[0].SKU() != "B-1" {
		t.Fatalf("only reserved = %v err=%v", page, err)
	}

	// ── reservation: add, duplicate, round trip with shortfall, update ─────
	line := func(sku string, qty int32) reservation.Line {
		l, err := reservation.NewLine(sku, qty)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	if _, err := reservations.FindByOrderID(c, "o-1"); !errors.Is(err, reservation.ErrNotFound) {
		t.Fatalf("missing reservation: %v", err)
	}
	short, err := reservation.Reserve(reservation.Params{
		OrderID: "o-1",
		Lines:   []reservation.Line{line("A", 1), line("Z", 2), line("A", 9)},
		Now:     now,
	}, map[string]*stock.Stock{"A": reloaded})
	if err != nil || short.Status() != reservation.StatusRejected {
		t.Fatalf("reserve = %v %v", short, err)
	}
	if err := reservations.Add(c, short); err != nil {
		t.Fatal(err)
	}
	if err := reservations.Add(c, short); !errors.Is(err, reservation.ErrDuplicate) {
		t.Fatalf("duplicate add: %v", err)
	}
	got, err := reservations.FindByOrderID(c, "o-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status() != reservation.StatusRejected || len(got.Lines()) != 3 || got.Lines()[2].Quantity() != 9 || !got.CreatedAt().Equal(now) {
		t.Fatalf("round trip = %+v", got)
	}
	if sf := got.Shortfall(); len(sf) != 2 || sf[0] != "A" || sf[1] != "Z" {
		t.Fatalf("shortfall = %v, want [A Z] (distinct, in line order)", sf)
	}

	ok, err := reservation.Reserve(reservation.Params{OrderID: "o-2", Lines: []reservation.Line{line("A", 2)}, Now: now}, map[string]*stock.Stock{"A": reloaded})
	if err != nil || ok.Status() != reservation.StatusReserved {
		t.Fatalf("reserve ok = %v %v", ok, err)
	}
	if err := reservations.Add(c, ok); err != nil {
		t.Fatal(err)
	}
	// Update is a compare-and-swap on the status the aggregate was loaded with:
	// two copies read as RESERVED, the first to write wins, the other is stale.
	ok, err = reservations.FindByOrderID(c, "o-2")
	if err != nil {
		t.Fatal(err)
	}
	rival, _ := reservations.FindByOrderID(c, "o-2")
	if err := ok.Release(map[string]*stock.Stock{"A": reloaded}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := reservations.Update(c, ok); err != nil {
		t.Fatal(err)
	}
	// rival ran in its own transaction, against stock as it was when it read.
	own, _ := stock.Reconstitute(stock.ReconstituteParams{SKU: "A", OnHand: 5, Reserved: 2, UpdatedAt: now})
	if err := rival.Expire(map[string]*stock.Stock{"A": own}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := reservations.Update(c, rival); !errors.Is(err, reservation.ErrStale) || !errors.Is(err, kernel.ErrConcurrentUpdate) {
		t.Fatalf("update over a moved reservation: %v", err)
	}
	got, _ = reservations.FindByOrderID(c, "o-2")
	if got.Status() != reservation.StatusReleased || !got.UpdatedAt().Equal(now.Add(time.Second)) || len(got.Shortfall()) != 0 {
		t.Fatalf("after update = %+v", got)
	}
	never, _ := reservation.Reserve(reservation.Params{OrderID: "o-3", Lines: []reservation.Line{line("A", 1)}, Now: now}, nil)
	if err := reservations.Update(c, never); !errors.Is(err, reservation.ErrNotFound) {
		t.Fatalf("update unknown: %v", err)
	}
	// A tombstone (CANCELLED, no lines) round-trips: zero line rows is a valid aggregate.
	tomb, _ := reservation.Tombstone("o-tomb", now)
	if err := reservations.Add(c, tomb); err != nil {
		t.Fatalf("add tombstone: %v", err)
	}
	got, err = reservations.FindByOrderID(c, "o-tomb")
	if err != nil || got.Status() != reservation.StatusCancelled || len(got.Lines()) != 0 || len(got.Shortfall()) != 0 {
		t.Fatalf("tombstone round trip = %+v, %v", got, err)
	}

	// ── list: newest first, order_id breaks the created_at tie, lines preloaded ──
	// o-1 (REJECTED), o-2 (RELEASED) and o-tomb (CANCELLED) all share `now`.
	rpage, rnext, err := reservations.List(c, reservation.ListParams{Limit: 2})
	if err != nil || len(rpage) != 2 || rpage[0].OrderID() != "o-tomb" || rpage[1].OrderID() != "o-2" || rnext == nil || rnext.OrderID() != "o-2" {
		t.Fatalf("page 1 = %v next=%v err=%v", ids(rpage), rnext, err)
	}
	rpage, rnext, err = reservations.List(c, reservation.ListParams{Limit: 2, After: rnext})
	if err != nil || len(rpage) != 1 || rpage[0].OrderID() != "o-1" || rnext != nil {
		t.Fatalf("page 2 = %v next=%v err=%v", ids(rpage), rnext, err)
	}
	// ── count by status: REJECTED, RELEASED, CANCELLED one each; RESERVED absent ──
	counts, err := reservations.CountByStatus(c)
	if err != nil || len(counts) != 3 {
		t.Fatalf("CountByStatus = %+v, %v", counts, err)
	}
	for _, sc := range counts {
		if sc.Count() != 1 || sc.Status() == reservation.StatusReserved || sc.OldestUpdatedAt().IsZero() {
			t.Fatalf("CountByStatus entry = %+v", sc)
		}
	}
	if len(rpage[0].Lines()) == 0 || len(rpage[0].Shortfall()) == 0 {
		t.Fatalf("listed reservation lost its lines: %+v", rpage[0])
	}
	cancelled := reservation.StatusCancelled
	rpage, rnext, err = reservations.List(c, reservation.ListParams{Status: &cancelled, Limit: 10})
	if err != nil || len(rpage) != 1 || rpage[0].OrderID() != "o-tomb" || rnext != nil {
		t.Fatalf("filtered = %v next=%v err=%v", ids(rpage), rnext, err)
	}

	// ── reserved before: RESERVED only, hold started before the instant, oldest first ──
	// o-2 was RESERVED then RELEASED above, so it must not show; o-old (reserved an hour
	// before `now`) and o-new (at `now`) are the candidates.
	reloaded, _ = stocks.FindBySKU(c, "A")
	for _, in := range []struct {
		id string
		at time.Time
	}{{"o-new", now}, {"o-old", now.Add(-time.Hour)}} {
		r, err := reservation.Reserve(reservation.Params{OrderID: in.id, Lines: []reservation.Line{line("A", 1)}, Now: in.at}, map[string]*stock.Stock{"A": reloaded})
		if err != nil || r.Status() != reservation.StatusReserved {
			t.Fatalf("reserve %s = %v %v", in.id, r, err)
		}
		if err := reservations.Add(c, r); err != nil {
			t.Fatal(err)
		}
	}
	due, err := reservations.ListReservedBefore(c, now.Add(time.Second), 10)
	if err != nil || len(due) != 2 || due[0].OrderID() != "o-old" || due[1].OrderID() != "o-new" || len(due[0].Lines()) != 1 {
		t.Fatalf("reserved before now+1s = %v, %v", ids(due), err)
	}
	due, err = reservations.ListReservedBefore(c, now, 10)
	if err != nil || len(due) != 1 || due[0].OrderID() != "o-old" {
		t.Fatalf("reserved before now = %v, %v (strictly before)", ids(due), err)
	}
	due, err = reservations.ListReservedBefore(c, now.Add(time.Second), 1)
	if err != nil || len(due) != 1 || due[0].OrderID() != "o-old" {
		t.Fatalf("limit 1 = %v, %v", ids(due), err)
	}

	// ── find by order ids: one query, lines preloaded, unknown and repeated ids tolerated ──
	found, err := reservations.FindByOrderIDs(c, []string{"o-1", "o-missing", "o-old", "o-1"})
	if err != nil || len(found) != 2 {
		t.Fatalf("FindByOrderIDs = %v, %v; want o-1 and o-old", ids(found), err)
	}
	for _, r := range found {
		if len(r.Lines()) == 0 {
			t.Fatalf("FindByOrderIDs lost the lines of %s", r.OrderID())
		}
	}
	if found, err := reservations.FindByOrderIDs(c, []string{"o-missing"}); err != nil || len(found) != 0 {
		t.Fatalf("FindByOrderIDs unknown = %v, %v", ids(found), err)
	}
}

func ids(rs []*reservation.Reservation) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.OrderID())
	}
	return out
}
