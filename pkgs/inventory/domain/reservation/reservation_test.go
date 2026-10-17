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
	if _, err := reservation.Reconstitute(reservation.ReconstituteParams{OrderID: "o", Lines: lines, Status: 0}); !errors.Is(err, reservation.ErrInvalidStatus) {
		t.Fatalf("bad status: %v", err)
	}
}

func TestStatus(t *testing.T) {
	for _, s := range []reservation.Status{reservation.StatusReserved, reservation.StatusRejected, reservation.StatusReleased} {
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
