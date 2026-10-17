package order_test

import (
	"errors"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
)

func TestNewCursor(t *testing.T) {
	at := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)

	cur, err := order.NewCursor(at, "o-1")
	if err != nil || !cur.CreatedAt().Equal(at) || cur.ID() != "o-1" {
		t.Fatalf("NewCursor = %+v, %v", cur, err)
	}

	for name, in := range map[string]struct {
		at time.Time
		id order.ID
	}{"zero time": {time.Time{}, "o-1"}, "empty id": {at, ""}} {
		if _, err := order.NewCursor(in.at, in.id); !errors.Is(err, order.ErrInvalidCursor) {
			t.Errorf("%s: err = %v, want ErrInvalidCursor", name, err)
		}
	}
}

func TestCursor_Before(t *testing.T) {
	at := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	usd, _ := kernel.NewMoney(100, "USD")
	item, _ := order.NewLineItem("SKU", 1, usd)
	o, err := order.New(order.Params{ID: "o-5", CustomerID: "c-1", Items: []order.LineItem{item}, Now: at})
	if err != nil {
		t.Fatal(err)
	}
	cur := order.CursorOf(o)
	if !cur.CreatedAt().Equal(at) || cur.ID() != "o-5" {
		t.Fatalf("CursorOf = %+v", cur)
	}

	// Listing order is (created_at desc, id desc): a row is "after" the cursor
	// when it is older, or equally old with a smaller id.
	tests := []struct {
		name string
		at   time.Time
		id   order.ID
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
