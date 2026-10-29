package reservation_test

import (
	"errors"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/stock"
)

var now = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func line(t *testing.T, sku string, qty int32) reservation.Line {
	t.Helper()
	l, err := reservation.NewLine(sku, qty)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func stocks(t *testing.T, onHand map[string]int32) map[string]*stock.Stock {
	t.Helper()
	out := make(map[string]*stock.Stock, len(onHand))
	for sku, n := range onHand {
		s, err := stock.New(stock.SKU(sku), n, now)
		if err != nil {
			t.Fatal(err)
		}
		out[sku] = s
	}
	return out
}

func TestNewLine(t *testing.T) {
	l := line(t, "A", 2)
	if l.SKU() != "A" || l.Quantity() != 2 {
		t.Fatalf("line = %+v", l)
	}
	if _, err := reservation.NewLine("", 1); !errors.Is(err, reservation.ErrInvalidSKU) {
		t.Fatalf("empty sku: %v", err)
	}
	if _, err := reservation.NewLine("A", 0); !errors.Is(err, reservation.ErrInvalidQuantity) {
		t.Fatalf("zero qty: %v", err)
	}
}

func TestReserve_Validation(t *testing.T) {
	if _, err := reservation.Reserve(reservation.Params{Lines: []reservation.Line{line(t, "A", 1)}, Now: now}, nil); !errors.Is(err, reservation.ErrInvalidOrderID) {
		t.Fatalf("empty order id: %v", err)
	}
	if _, err := reservation.Reserve(reservation.Params{OrderID: "o-1", Now: now}, nil); !errors.Is(err, reservation.ErrNoLines) {
		t.Fatalf("no lines: %v", err)
	}
}

func TestReserve_Reserved(t *testing.T) {
	st := stocks(t, map[string]int32{"A": 5, "B": 1})
	// A listed twice: the sum (3) must fit.
	r, err := reservation.Reserve(reservation.Params{
		OrderID: "o-1",
		Lines:   []reservation.Line{line(t, "A", 1), line(t, "B", 1), line(t, "A", 2)},
		Now:     now,
	}, st)
	if err != nil {
		t.Fatal(err)
	}
	if r.OrderID() != "o-1" || r.Status() != reservation.StatusReserved || len(r.Lines()) != 3 || len(r.Shortfall()) != 0 {
		t.Fatalf("reservation = %+v", r)
	}
	if !r.CreatedAt().Equal(now) || !r.UpdatedAt().Equal(now) {
		t.Fatal("timestamps")
	}
	if st["A"].Reserved() != 3 || st["B"].Reserved() != 1 {
		t.Fatalf("stock A=%d B=%d", st["A"].Reserved(), st["B"].Reserved())
	}

	evs := r.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	ev, ok := evs[0].(reservation.Reserved)
	if !ok || ev.OrderID() != "o-1" || ev.AggregateID() != "o-1" || !ev.OccurredAt().Equal(now) || len(ev.Lines()) != 3 {
		t.Fatalf("event = %#v", evs[0])
	}
	if len(r.PullEvents()) != 0 {
		t.Fatal("PullEvents must clear")
	}
}

func TestReserve_Rejected(t *testing.T) {
	st := stocks(t, map[string]int32{"A": 5, "B": 1})
	r, err := reservation.Reserve(reservation.Params{
		OrderID: "o-2",
		Lines:   []reservation.Line{line(t, "A", 1), line(t, "B", 2), line(t, "C", 1)},
		Now:     now,
	}, st)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status() != reservation.StatusRejected {
		t.Fatalf("status = %s", r.Status())
	}
	if sf := r.Shortfall(); len(sf) != 2 || sf[0] != "B" || sf[1] != "C" {
		t.Fatalf("shortfall = %v", sf)
	}
	if st["A"].Reserved() != 0 || st["B"].Reserved() != 0 {
		t.Fatal("rejected reservation must not touch stock")
	}
	evs := r.PullEvents()
	ev, ok := evs[0].(reservation.Rejected)
	if len(evs) != 1 || !ok || ev.OrderID() != "o-2" || len(ev.Shortfall()) != 2 {
		t.Fatalf("events = %#v", evs)
	}

	// A rejected reservation holds nothing: releasing it is a final error.
	if err := r.Release(st, now); !errors.Is(err, reservation.ErrNotReserved) {
		t.Fatalf("release rejected: %v", err)
	}
}

func TestRelease(t *testing.T) {
	st := stocks(t, map[string]int32{"A": 5, "B": 1})
	lines := []reservation.Line{line(t, "A", 2), line(t, "B", 1)}
	r, err := reservation.Reserve(reservation.Params{OrderID: "o-3", Lines: lines, Now: now}, st)
	if err != nil {
		t.Fatal(err)
	}
	r.PullEvents()
	later := now.Add(time.Minute)

	// Stock missing: nothing changes.
	if err := r.Release(map[string]*stock.Stock{"A": st["A"]}, later); !errors.Is(err, reservation.ErrStockMissing) {
		t.Fatalf("missing stock: %v", err)
	}
	if r.Status() != reservation.StatusReserved || st["A"].Reserved() != 2 {
		t.Fatal("failed release must not change anything")
	}

	// Stock says less is reserved than the reservation holds (corrupt): the stock error surfaces.
	bad, _ := stock.Reconstitute(stock.ReconstituteParams{SKU: "A", OnHand: 5, Reserved: 1, UpdatedAt: now})
	if err := r.Release(map[string]*stock.Stock{"A": bad, "B": st["B"]}, later); !errors.Is(err, stock.ErrReleaseExceedsReserved) {
		t.Fatalf("corrupt stock: %v", err)
	}

	if err := r.Release(st, later); err != nil {
		t.Fatal(err)
	}
	if r.Status() != reservation.StatusReleased || !r.UpdatedAt().Equal(later) || st["A"].Reserved() != 0 || st["B"].Reserved() != 0 {
		t.Fatalf("status=%s A=%d B=%d", r.Status(), st["A"].Reserved(), st["B"].Reserved())
	}
	evs := r.PullEvents()
	ev, ok := evs[0].(reservation.Released)
	if len(evs) != 1 || !ok || ev.OrderID() != "o-3" || !ev.OccurredAt().Equal(later) {
		t.Fatalf("events = %#v", evs)
	}
	if err := r.Release(st, later); !errors.Is(err, reservation.ErrNotReserved) {
		t.Fatalf("double release: %v", err)
	}
}

func TestCommit(t *testing.T) {
	st := stocks(t, map[string]int32{"A": 5, "B": 1})
	lines := []reservation.Line{line(t, "A", 2), line(t, "B", 1)}
	r, err := reservation.Reserve(reservation.Params{OrderID: "o-4", Lines: lines, Now: now}, st)
	if err != nil {
		t.Fatal(err)
	}
	r.PullEvents()
	later := now.Add(time.Minute)

	// Stock missing: nothing changes.
	if err := r.Commit(map[string]*stock.Stock{"A": st["A"]}, later); !errors.Is(err, reservation.ErrStockMissing) {
		t.Fatalf("missing stock: %v", err)
	}
	if r.Status() != reservation.StatusReserved || st["A"].Reserved() != 2 || st["A"].OnHand() != 5 {
		t.Fatal("failed commit must not change anything")
	}

	// Stock says less is reserved than the reservation holds (corrupt): the stock error surfaces.
	bad, _ := stock.Reconstitute(stock.ReconstituteParams{SKU: "A", OnHand: 5, Reserved: 1, UpdatedAt: now})
	if err := r.Commit(map[string]*stock.Stock{"A": bad, "B": st["B"]}, later); !errors.Is(err, stock.ErrCommitExceedsReserved) {
		t.Fatalf("corrupt stock: %v", err)
	}

	if err := r.Commit(st, later); err != nil {
		t.Fatal(err)
	}
	// Sold: on-hand drops by what was reserved, nothing stays reserved.
	if r.Status() != reservation.StatusCommitted || !r.UpdatedAt().Equal(later) ||
		st["A"].OnHand() != 3 || st["A"].Reserved() != 0 || st["B"].OnHand() != 0 || st["B"].Reserved() != 0 {
		t.Fatalf("status=%s A=%d/%d B=%d/%d", r.Status(), st["A"].OnHand(), st["A"].Reserved(), st["B"].OnHand(), st["B"].Reserved())
	}
	evs := r.PullEvents()
	ev, ok := evs[0].(reservation.Committed)
	if len(evs) != 1 || !ok || ev.OrderID() != "o-4" || ev.AggregateID() != "o-4" || !ev.OccurredAt().Equal(later) {
		t.Fatalf("events = %#v", evs)
	}
	// Terminal: neither a redelivered OrderCompleted nor a late OrderCancelled changes it.
	if err := r.Commit(st, later); !errors.Is(err, reservation.ErrNotReserved) {
		t.Fatalf("double commit: %v", err)
	}
	if err := r.Release(st, later); !errors.Is(err, reservation.ErrNotReserved) {
		t.Fatalf("release after commit: %v", err)
	}
}

// expired reserves lines against stocks and lets the sweep give them back, as
// when OrderCompleted is still in flight (issue #218).
func expired(t *testing.T, st map[string]*stock.Stock, lines ...reservation.Line) *reservation.Reservation {
	t.Helper()
	r, err := reservation.Reserve(reservation.Params{OrderID: "o-6", Lines: lines, Now: now}, st)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Expire(st, now.Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	r.PullEvents()

	return r
}

func TestCommit_AfterExpire(t *testing.T) {
	later := now.Add(26 * time.Hour)

	t.Run("covered: sold from what is available", func(t *testing.T) {
		st := stocks(t, map[string]int32{"A": 5, "B": 1})
		r := expired(t, st, line(t, "A", 2), line(t, "B", 1))

		if err := r.Commit(map[string]*stock.Stock{"A": st["A"]}, later); !errors.Is(err, reservation.ErrStockMissing) {
			t.Fatalf("missing stock: %v", err)
		}
		if err := r.Commit(st, later); err != nil {
			t.Fatal(err)
		}
		if r.Status() != reservation.StatusCommitted || !r.UpdatedAt().Equal(later) ||
			st["A"].OnHand() != 3 || st["A"].Reserved() != 0 || st["B"].OnHand() != 0 || st["B"].Reserved() != 0 {
			t.Fatalf("status=%s A=%d/%d B=%d/%d", r.Status(), st["A"].OnHand(), st["A"].Reserved(), st["B"].OnHand(), st["B"].Reserved())
		}
		if evs := r.PullEvents(); len(evs) != 1 {
			t.Fatalf("events = %#v", evs)
		} else if _, ok := evs[0].(reservation.Committed); !ok {
			t.Fatalf("event = %T", evs[0])
		}
		if err := r.Commit(st, later); !errors.Is(err, reservation.ErrNotReserved) {
			t.Fatalf("double commit: %v", err)
		}
	})

	t.Run("one line short: all or nothing, COMMIT_SHORT", func(t *testing.T) {
		st := stocks(t, map[string]int32{"A": 5, "B": 1})
		r := expired(t, st, line(t, "A", 2), line(t, "B", 1))
		// Someone else claimed B's last unit after the sweep.
		if _, err := reservation.Reserve(reservation.Params{OrderID: "o-7", Lines: []reservation.Line{line(t, "B", 1)}, Now: now}, st); err != nil {
			t.Fatal(err)
		}

		if err := r.Commit(st, later); err != nil {
			t.Fatal(err)
		}
		if r.Status() != reservation.StatusCommitShort || !r.UpdatedAt().Equal(later) ||
			st["A"].OnHand() != 5 || st["A"].Reserved() != 0 || st["B"].OnHand() != 1 || st["B"].Reserved() != 1 {
			t.Fatalf("status=%s A=%d/%d B=%d/%d", r.Status(), st["A"].OnHand(), st["A"].Reserved(), st["B"].OnHand(), st["B"].Reserved())
		}
		if evs := r.PullEvents(); len(evs) != 0 {
			t.Fatalf("events = %#v", evs)
		}
		// COMMIT_SHORT is terminal.
		for name, f := range map[string]func() error{
			"commit":  func() error { return r.Commit(st, later) },
			"release": func() error { return r.Release(st, later) },
			"expire":  func() error { return r.Expire(st, later) },
		} {
			if err := f(); !errors.Is(err, reservation.ErrNotReserved) {
				t.Fatalf("%s after commit short: %v", name, err)
			}
		}
	})
}

func TestExpire(t *testing.T) {
	st := stocks(t, map[string]int32{"A": 5, "B": 1})
	lines := []reservation.Line{line(t, "A", 2), line(t, "B", 1)}
	r, err := reservation.Reserve(reservation.Params{OrderID: "o-5", Lines: lines, Now: now}, st)
	if err != nil {
		t.Fatal(err)
	}
	r.PullEvents()
	later := now.Add(25 * time.Hour)

	// Same preconditions as Release: a missing stock changes nothing.
	if err := r.Expire(map[string]*stock.Stock{"A": st["A"]}, later); !errors.Is(err, reservation.ErrStockMissing) {
		t.Fatalf("missing stock: %v", err)
	}
	if r.Status() != reservation.StatusReserved || st["A"].Reserved() != 2 {
		t.Fatal("failed expire must not change anything")
	}
	// Stock says less is reserved than the reservation holds (corrupt): the stock error surfaces.
	bad, _ := stock.Reconstitute(stock.ReconstituteParams{SKU: "A", OnHand: 5, Reserved: 1, UpdatedAt: now})
	if err := r.Expire(map[string]*stock.Stock{"A": bad, "B": st["B"]}, later); !errors.Is(err, stock.ErrReleaseExceedsReserved) {
		t.Fatalf("corrupt stock: %v", err)
	}

	if err := r.Expire(st, later); err != nil {
		t.Fatal(err)
	}
	// Given back, not sold: on-hand untouched, nothing reserved.
	if r.Status() != reservation.StatusExpired || !r.UpdatedAt().Equal(later) ||
		st["A"].OnHand() != 5 || st["A"].Reserved() != 0 || st["B"].OnHand() != 1 || st["B"].Reserved() != 0 {
		t.Fatalf("status=%s A=%d/%d B=%d/%d", r.Status(), st["A"].OnHand(), st["A"].Reserved(), st["B"].OnHand(), st["B"].Reserved())
	}
	// Released (the stock moved) then Expired (ask ordering to cancel), both at `later`.
	evs := r.PullEvents()
	if len(evs) != 2 {
		t.Fatalf("events = %#v", evs)
	}
	if rel, ok := evs[0].(reservation.Released); !ok || rel.OrderID() != "o-5" || !rel.OccurredAt().Equal(later) {
		t.Fatalf("events[0] = %#v", evs[0])
	}
	exp, ok := evs[1].(reservation.Expired)
	if !ok || exp.OrderID() != "o-5" || exp.AggregateID() != "o-5" || !exp.OccurredAt().Equal(later) || exp.HeldFor() != 25*time.Hour {
		t.Fatalf("events[1] = %#v", evs[1])
	}
	// A late OrderCancelled / second sweep refuse; OrderCompleted does not (TestCommit_AfterExpire).
	for name, f := range map[string]func() error{
		"release": func() error { return r.Release(st, later) },
		"expire":  func() error { return r.Expire(st, later) },
	} {
		if err := f(); !errors.Is(err, reservation.ErrNotReserved) {
			t.Fatalf("%s after expire: %v", name, err)
		}
	}
}

func TestReconstitute(t *testing.T) {
	lines := []reservation.Line{line(t, "A", 1)}
	r, err := reservation.Reconstitute(reservation.ReconstituteParams{
		OrderID: "o-4", Lines: lines, Status: reservation.StatusRejected, Shortfall: []string{"A"}, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status() != reservation.StatusRejected || r.Shortfall()[0] != "A" || len(r.PullEvents()) != 0 {
		t.Fatalf("reconstituted = %+v", r)
	}

	if _, err := reservation.Reconstitute(reservation.ReconstituteParams{Lines: lines, Status: reservation.StatusReserved}); !errors.Is(err, reservation.ErrInvalidOrderID) {
		t.Fatalf("empty id: %v", err)
	}
	if _, err := reservation.Reconstitute(reservation.ReconstituteParams{OrderID: "o", Status: reservation.StatusReserved}); !errors.Is(err, reservation.ErrNoLines) {
		t.Fatalf("no lines: %v", err)
	}
	// A tombstone is the one reservation allowed to have no lines.
	tomb, err := reservation.Reconstitute(reservation.ReconstituteParams{OrderID: "o", Status: reservation.StatusCancelled, CreatedAt: now, UpdatedAt: now})
	if err != nil || tomb.Status() != reservation.StatusCancelled || len(tomb.Lines()) != 0 {
		t.Fatalf("tombstone reconstitute = %+v, %v", tomb, err)
	}
	if _, err := reservation.Reconstitute(reservation.ReconstituteParams{OrderID: "o", Lines: lines, Status: 0}); !errors.Is(err, reservation.ErrInvalidStatus) {
		t.Fatalf("bad status: %v", err)
	}
}

func TestTombstone(t *testing.T) {
	if _, err := reservation.Tombstone("", now); !errors.Is(err, reservation.ErrInvalidOrderID) {
		t.Fatalf("empty id: %v", err)
	}
	r, err := reservation.Tombstone("o-late", now)
	if err != nil {
		t.Fatal(err)
	}
	if r.OrderID() != "o-late" || r.Status() != reservation.StatusCancelled || len(r.Lines()) != 0 || len(r.Shortfall()) != 0 {
		t.Fatalf("tombstone = %+v", r)
	}
	if !r.CreatedAt().Equal(now) || !r.UpdatedAt().Equal(now) {
		t.Fatalf("timestamps = %v / %v", r.CreatedAt(), r.UpdatedAt())
	}
	// Nothing was reserved: no event, and a (redelivered) release is a final no.
	if len(r.PullEvents()) != 0 {
		t.Fatal("tombstone must not raise events")
	}
	if err := r.Release(map[string]*stock.Stock{}, now); !errors.Is(err, reservation.ErrNotReserved) {
		t.Fatalf("release tombstone: %v", err)
	}
}

func TestStatus(t *testing.T) {
	for _, s := range reservation.Statuses() {
		got, err := reservation.ParseStatus(s.String())
		if err != nil || got != s {
			t.Fatalf("round trip %s: %v %v", s, got, err)
		}
	}
	if reservation.Status(0).String() != "UNKNOWN" {
		t.Fatal("zero status must print UNKNOWN")
	}
	if _, err := reservation.ParseStatus("nope"); !errors.Is(err, reservation.ErrInvalidStatus) {
		t.Fatalf("parse unknown: %v", err)
	}
}
