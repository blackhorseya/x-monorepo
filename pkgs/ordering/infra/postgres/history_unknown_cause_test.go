package postgres

import (
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
)

// A stored cause this version does not know (a row written by a newer release, or by hand) must not
// make the whole history unreadable: it reads back as UNSPECIFIED, every other column intact.
func TestToHistoryEntry_UnknownCauseReadsAsUnspecified(t *testing.T) {
	at := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	from := "CONFIRMED"
	row := func(cause string) historyRow {
		return historyRow{
			OrderID: "o-1", Seq: 3, FromStatus: &from, ToStatus: "CANCELLED", OccurredAt: at,
			ActorKind: "SYSTEM", Cause: cause,
		}
	}

	// Control: a known cause round-trips, so the case below fails for the cause alone.
	known, err := toHistoryEntry(row("STOCK_RESERVED"))
	if err != nil || known.Cause() != order.CauseStockReserved {
		t.Fatalf("known cause = %v, %v", known.Cause(), err)
	}

	got, err := toHistoryEntry(row("A_CAUSE_FROM_THE_FUTURE"))
	if err != nil {
		t.Fatalf("unknown cause made the row unreadable: %v", err)
	}
	if got.Cause() != order.CauseUnspecified || got.Seq() != 3 || got.To() != order.StatusCancelled || got.From() != order.StatusConfirmed || got.ActorKind() != order.ActorSystem || !got.OccurredAt().Equal(at) {
		t.Fatalf("entry = %+v, want only the cause degraded to UNSPECIFIED", got)
	}

	// An unknown status is still corrupt data, not something to paper over.
	bad := row("RPC_REQUEST")
	bad.ToStatus = "NOPE"
	if _, err := toHistoryEntry(bad); err == nil {
		t.Fatal("an unknown status must stay an error")
	}
}
