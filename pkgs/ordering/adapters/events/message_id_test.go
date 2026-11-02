package events_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/events"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
)

// Every published message carries a fresh UUID v7 id; the id is an opaque
// dedup key for consumers, never an ordering.
func TestPublisher_MessageIDIsV7(t *testing.T) {
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	usd, _ := kernel.NewMoney(250, "USD")
	li, _ := order.NewLineItem("SKU-1", 2, usd)
	o, err := order.New(order.Params{ID: "o-1", CustomerID: "c-1", Items: []order.LineItem{li}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Confirm(now.Add(time.Second), false); err != nil {
		t.Fatal(err)
	}

	sink := &capture{}
	if err := events.NewPublisher(sink).Publish(context.Background(), o.PullEvents()...); err != nil {
		t.Fatal(err)
	}
	if len(sink.msgs) < 2 {
		t.Fatalf("published %d messages, want at least 2", len(sink.msgs))
	}
	seen := map[string]bool{}
	for _, m := range sink.msgs {
		u, err := uuid.Parse(m.UUID)
		if err != nil || u.Version() != 7 {
			t.Fatalf("message id %q: version %d, %v; want v7", m.UUID, u.Version(), err)
		}
		if seen[m.UUID] {
			t.Fatalf("duplicate message id %q", m.UUID)
		}
		seen[m.UUID] = true
	}
}
