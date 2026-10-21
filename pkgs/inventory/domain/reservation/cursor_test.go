package reservation_test

import (
	"errors"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
)

func TestNewCursor(t *testing.T) {
	at := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)

	cur, err := reservation.NewCursor(at, "o-1")
	if err != nil || !cur.CreatedAt().Equal(at) || cur.OrderID() != "o-1" {
		t.Fatalf("NewCursor = %+v, %v", cur, err)
	}
	for name, in := range map[string]struct {
		at time.Time
		id string
	}{"zero time": {time.Time{}, "o-1"}, "empty id": {at, ""}} {
		if _, err := reservation.NewCursor(in.at, in.id); !errors.Is(err, reservation.ErrInvalidCursor) {
			t.Errorf("%s: err = %v, want ErrInvalidCursor", name, err)
		}
	}
}

func TestCursor_Before(t *testing.T) {
	at := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	r, err := reservation.Tombstone("o-5", at)
	if err != nil {
		t.Fatal(err)
	}
	cur := reservation.CursorOf(r)
	if !cur.CreatedAt().Equal(at) || cur.OrderID() != "o-5" {
		t.Fatalf("CursorOf = %+v", cur)
	}

	tests := []struct {
		name string
		at   time.Time
		id   string
		want bool
	}{
		{"older", at.Add(-time.Second), "o-9", true},
		{"newer", at.Add(time.Second), "o-1", false},
		{"same time, smaller id", at, "o-4", true},
		{"same time, same id", at, "o-5", false},
		{"same time, larger id", at, "o-6", false},
	}
	for _, tt := range tests {
		if got := cur.Before(tt.at, tt.id); got != tt.want {
			t.Errorf("%s: Before = %v, want %v", tt.name, got, tt.want)
		}
	}
}
