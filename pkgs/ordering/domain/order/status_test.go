package order_test

import (
	"errors"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
)

func TestStatuses_AndStatusCount(t *testing.T) {
	if got := order.Statuses(); len(got) != 3 || got[0] != order.StatusPending || got[2] != order.StatusCancelled {
		t.Fatalf("Statuses() = %v", got)
	}
	at := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	sc := order.NewStatusCount(order.StatusPending, 4, at)
	if sc.Status() != order.StatusPending || sc.Count() != 4 || !sc.OldestUpdatedAt().Equal(at) {
		t.Fatalf("StatusCount = %+v", sc)
	}
}

func TestParseStatus_RoundTrip(t *testing.T) {
	for _, s := range []order.Status{order.StatusPending, order.StatusConfirmed, order.StatusCancelled} {
		got, err := order.ParseStatus(s.String())
		if err != nil || got != s {
			t.Fatalf("ParseStatus(%q) = %v, %v", s, got, err)
		}
	}
	if _, err := order.ParseStatus("UNKNOWN"); !errors.Is(err, order.ErrInvalidStatus) {
		t.Fatalf("unknown name: err = %v", err)
	}
	if _, err := order.ParseStatus(""); !errors.Is(err, order.ErrInvalidStatus) {
		t.Fatalf("empty name: err = %v", err)
	}
}
