package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
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
