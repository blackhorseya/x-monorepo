package events_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/adapters/events"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/stock"
)

// Every published message carries a fresh UUID v7 id; the id is an opaque
// dedup key for consumers, never an ordering.
func TestPublisher_MessageIDIsV7(t *testing.T) {
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	a, _ := stock.New("A", 5, now)
	l, err := reservation.NewLine("A", 2)
	if err != nil {
		t.Fatal(err)
	}
	r, err := reservation.Reserve(reservation.Params{OrderID: "o-1", Lines: []reservation.Line{l}, Now: now}, map[string]*stock.Stock{"A": a})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Release(map[string]*stock.Stock{"A": a}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	sink := &capture{}
	if err := events.NewPublisher(sink).Publish(context.Background(), r.PullEvents()...); err != nil {
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
