package usecase_test

import (
	"context"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/memory"
)

// Every known status is reported, zero when empty, so a gauge series never vanishes.
func TestReservationStats_ZeroFills(t *testing.T) {
	c := context.Background()
	repo := memory.NewReservationRepository()
	stats := usecase.NewReservationStats(repo)

	out, err := stats.Execute(c, dto.ReservationStatsInput{})
	if err != nil || len(out.Statuses) != 5 {
		t.Fatalf("empty stats = %+v, %v", out, err)
	}
	for _, s := range out.Statuses {
		if s.Count != 0 || !s.OldestUpdatedAt.IsZero() {
			t.Fatalf("empty status %s = %+v", s.Status, s)
		}
	}

	t0 := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	for i, id := range []string{"o-1", "o-2"} {
		r, _ := reservation.Tombstone(id, t0.Add(time.Duration(i)*time.Minute))
		if err := repo.Add(c, r); err != nil {
			t.Fatal(err)
		}
	}
	out, err = stats.Execute(c, dto.ReservationStatsInput{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range out.Statuses {
		switch s.Status {
		case "CANCELLED":
			if s.Count != 2 || !s.OldestUpdatedAt.Equal(t0) {
				t.Fatalf("CANCELLED = %+v", s)
			}
		default:
			if s.Count != 0 {
				t.Fatalf("%s = %+v, want 0", s.Status, s)
			}
		}
	}
}
