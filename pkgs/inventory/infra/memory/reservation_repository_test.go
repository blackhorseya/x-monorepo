package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/stock"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/memory"
)

// List is newest first with order_id breaking created_at ties — the same
// contract as Postgres, so the console behaves alike with and without a DB.
func TestReservationRepository_List(t *testing.T) {
	c := context.Background()
	repo := memory.NewReservationRepository()
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	for _, in := range []struct {
		id string
		at time.Time
	}{{"o-b", now}, {"o-old", now.Add(-time.Hour)}, {"o-a", now}, {"o-c", now}} {
		r, _ := reservation.Tombstone(in.id, in.at)
		if err := repo.Add(c, r); err != nil {
			t.Fatal(err)
		}
	}
	page, next, err := repo.List(c, reservation.ListParams{Limit: 2})
	if err != nil || len(page) != 2 || page[0].OrderID() != "o-c" || page[1].OrderID() != "o-b" || next == nil || next.OrderID() != "o-b" {
		t.Fatalf("page 1 = %v next=%v err=%v", page, next, err)
	}
	page, next, err = repo.List(c, reservation.ListParams{Limit: 2, After: next})
	if err != nil || len(page) != 2 || page[0].OrderID() != "o-a" || page[1].OrderID() != "o-old" || next != nil {
		t.Fatalf("page 2 = %v next=%v err=%v", page, next, err)
	}
	reserved := reservation.StatusReserved
	if page, _, err := repo.List(c, reservation.ListParams{Status: &reserved, Limit: 10}); err != nil || len(page) != 0 {
		t.Fatalf("filtered = %v err=%v, want none", page, err)
	}
	// A cancelled context is reported, not ignored.
	cancelled, cancel := context.WithCancel(c)
	cancel()
	if _, _, err := repo.List(cancelled, reservation.ListParams{Limit: 1}); err == nil {
		t.Fatal("cancelled context must fail")
	}
}

// Update is a compare-and-swap on the status the aggregate was loaded with, like
// Postgres: of two copies read as RESERVED the first to write wins, the other is
// stale (retryable), and an unknown order is NotFound.
func TestReservationRepository_UpdateIsCompareAndSwap(t *testing.T) {
	c := context.Background()
	repo := memory.NewReservationRepository()
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	line, _ := reservation.NewLine("A", 1)
	st, _ := stock.New("A", 5, now)
	stocks := map[string]*stock.Stock{"A": st}
	r, err := reservation.Reserve(reservation.Params{OrderID: "o-1", Lines: []reservation.Line{line}, Now: now}, stocks)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Add(c, r); err != nil {
		t.Fatal(err)
	}
	first, _ := repo.FindByOrderID(c, "o-1")
	second, _ := repo.FindByOrderID(c, "o-1")
	if err := first.Release(stocks, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := repo.Update(c, first); err != nil {
		t.Fatal(err)
	}
	// second ran in its own transaction, against stock as it was when it read.
	own, _ := stock.Reconstitute(stock.ReconstituteParams{SKU: "A", OnHand: 5, Reserved: 1, UpdatedAt: now})
	if err := second.Expire(map[string]*stock.Stock{"A": own}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := repo.Update(c, second); !errors.Is(err, reservation.ErrStale) {
		t.Fatalf("update over a moved reservation: %v", err)
	}
	if got, _ := repo.FindByOrderID(c, "o-1"); got.Status() != reservation.StatusReleased {
		t.Fatalf("stored = %s, want the winner's RELEASED", got.Status())
	}
	never, _ := reservation.Tombstone("o-never", now)
	if err := repo.Update(c, never); !errors.Is(err, reservation.ErrNotFound) {
		t.Fatalf("update unknown: %v", err)
	}
}

// CountByStatus reports only the statuses that have reservations, with the oldest updated_at of each.
func TestReservationRepository_CountByStatus(t *testing.T) {
	c := context.Background()
	repo := memory.NewReservationRepository()
	t0 := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	if got, err := repo.CountByStatus(c); err != nil || len(got) != 0 {
		t.Fatalf("empty = %v, %v", got, err)
	}
	for i, id := range []string{"o-1", "o-2"} {
		r, _ := reservation.Tombstone(id, t0.Add(time.Duration(i)*time.Minute))
		if err := repo.Add(c, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := repo.CountByStatus(c)
	if err != nil || len(got) != 1 || got[0].Status() != reservation.StatusCancelled || got[0].Count() != 2 || !got[0].OldestUpdatedAt().Equal(t0) {
		t.Fatalf("counts = %+v, %v", got, err)
	}
}

// ListReservedBefore: RESERVED only, hold started strictly before the instant,
// oldest first, limited — the same contract as Postgres.
func TestReservationRepository_ListReservedBefore(t *testing.T) {
	c := context.Background()
	repo := memory.NewReservationRepository()
	now := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	line, _ := reservation.NewLine("A", 1)
	for _, in := range []struct {
		id string
		at time.Time
	}{{"o-new", now}, {"o-old", now.Add(-time.Hour)}, {"o-older", now.Add(-2 * time.Hour)}} {
		s, _ := stock.New("A", 10, in.at)
		r, err := reservation.Reserve(reservation.Params{OrderID: in.id, Lines: []reservation.Line{line}, Now: in.at}, map[string]*stock.Stock{"A": s})
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.Add(c, r); err != nil {
			t.Fatal(err)
		}
	}
	tomb, _ := reservation.Tombstone("o-tomb", now.Add(-3*time.Hour)) // not RESERVED: never due
	if err := repo.Add(c, tomb); err != nil {
		t.Fatal(err)
	}

	due, err := repo.ListReservedBefore(c, now, 10)
	if err != nil || len(due) != 2 || due[0].OrderID() != "o-older" || due[1].OrderID() != "o-old" {
		t.Fatalf("before now = %v, %v", due, err)
	}
	due, err = repo.ListReservedBefore(c, now, 1)
	if err != nil || len(due) != 1 || due[0].OrderID() != "o-older" {
		t.Fatalf("limit 1 = %v, %v", due, err)
	}
	// The copy handed out is detached from the store.
	if err := due[0].Expire(map[string]*stock.Stock{"A": mustStock(t, 10, 1)}, now); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.FindByOrderID(c, "o-older"); got.Status() != reservation.StatusReserved {
		t.Fatal("ListReservedBefore leaked the stored aggregate")
	}
	cancelled, cancel := context.WithCancel(c)
	cancel()
	if _, err := repo.ListReservedBefore(cancelled, now, 1); err == nil {
		t.Fatal("cancelled context must fail")
	}
}

func mustStock(t *testing.T, onHand, reserved int32) *stock.Stock {
	s, err := stock.Reconstitute(stock.ReconstituteParams{SKU: "A", OnHand: onHand, Reserved: reserved})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// FindByOrderIDs returns the reservations that exist, once each, whatever the
// order of the ids; unknown ids are skipped — the same contract as Postgres.
func TestReservationRepository_FindByOrderIDs(t *testing.T) {
	c := context.Background()
	repo := memory.NewReservationRepository()
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	for _, id := range []string{"o-1", "o-2", "o-3"} {
		r, _ := reservation.Tombstone(id, now)
		if err := repo.Add(c, r); err != nil {
			t.Fatal(err)
		}
	}
	rs, err := repo.FindByOrderIDs(c, []string{"o-3", "o-missing", "o-1", "o-3"})
	if err != nil || len(rs) != 2 {
		t.Fatalf("FindByOrderIDs = %v, %v; want o-1 and o-3", rs, err)
	}
	got := map[string]bool{}
	for _, r := range rs {
		got[r.OrderID()] = true
	}
	if !got["o-1"] || !got["o-3"] {
		t.Fatalf("FindByOrderIDs = %v", got)
	}
	if rs, err := repo.FindByOrderIDs(c, nil); err != nil || len(rs) != 0 {
		t.Fatalf("no ids = %v, %v", rs, err)
	}
	cancelled, cancel := context.WithCancel(c)
	cancel()
	if _, err := repo.FindByOrderIDs(cancelled, []string{"o-1"}); err == nil {
		t.Fatal("cancelled context must fail")
	}
}
