package reservation_test

import (
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
)

func TestStatuses_AndStatusCount(t *testing.T) {
	if got := reservation.Statuses(); len(got) != 7 || got[0] != reservation.StatusReserved || got[3] != reservation.StatusCommitted || got[4] != reservation.StatusExpired || got[5] != reservation.StatusCommitShort || got[6] != reservation.StatusCancelled {
		t.Fatalf("Statuses() = %v", got)
	}
	at := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	sc := reservation.NewStatusCount(reservation.StatusReserved, 2, at)
	if sc.Status() != reservation.StatusReserved || sc.Count() != 2 || !sc.OldestUpdatedAt().Equal(at) {
		t.Fatalf("StatusCount = %+v", sc)
	}
}
