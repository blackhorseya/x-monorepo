package memory_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/memory"
)

func newEntry(t *testing.T, id string, at time.Time) order.HistoryEntry {
	t.Helper()
	usd, _ := kernel.NewMoney(100, "USD")
	it, _ := order.NewLineItem("S", 1, usd)
	o, err := order.New(order.Params{ID: order.ID(id), CustomerID: "c-1", Items: []order.LineItem{it}, Now: at})
	if err != nil {
		t.Fatal(err)
	}
	h, err := order.NewHistoryEntry(o.PullEvents()[0], order.ActorSystem, "")
	if err != nil {
		t.Fatal(err)
	}

	return h
}

// Append numbers the rows of one order 1..N in call order, per order; List returns them by seq, as
// copies; an order with no rows lists as empty, not as an error.
func TestHistoryStore_SeqPerOrder(t *testing.T) {
	c := context.Background()
	s := memory.NewHistoryStore()
	at := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)

	if got, err := s.List(c, "o-1"); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("List of an unknown order = %v (nil=%v), %v", got, got == nil, err)
	}
	if err := s.Append(c, newEntry(t, "o-1", at), newEntry(t, "o-2", at), newEntry(t, "o-1", at.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(c); err != nil {
		t.Fatalf("empty Append: %v", err)
	}

	o1, err := s.List(c, "o-1")
	if err != nil || len(o1) != 2 || o1[0].Seq() != 1 || o1[1].Seq() != 2 || !o1[1].OccurredAt().Equal(at.Add(time.Minute)) {
		t.Fatalf("o-1 = %v, %v", o1, err)
	}
	o2, err := s.List(c, "o-2")
	if err != nil || len(o2) != 1 || o2[0].Seq() != 1 {
		t.Fatalf("o-2 = %v, %v", o2, err)
	}
}

func TestHistoryStore_ConcurrentAppendsStayContiguous(t *testing.T) {
	c := context.Background()
	s := memory.NewHistoryStore()
	at := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Append(c, newEntry(t, "o-1", at)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	got, err := s.List(c, "o-1")
	if err != nil || len(got) != 20 {
		t.Fatalf("rows = %d, %v; want 20", len(got), err)
	}
	for n, e := range got {
		if e.Seq() != n+1 {
			t.Fatalf("row %d has seq %d", n, e.Seq())
		}
	}
}

func TestHistoryStore_HonoursCancelledContext(t *testing.T) {
	c, cancel := context.WithCancel(context.Background())
	cancel()
	s := memory.NewHistoryStore()
	if err := s.Append(c, newEntry(t, "o-1", time.Now())); err == nil {
		t.Error("Append on a cancelled context succeeded")
	}
	if _, err := s.List(c, "o-1"); err == nil {
		t.Error("List on a cancelled context succeeded")
	}
}
