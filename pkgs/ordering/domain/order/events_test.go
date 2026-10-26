package order_test

import (
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
)

func TestOrder_Events(t *testing.T) {
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	usd, _ := kernel.NewMoney(250, "USD")
	a, _ := order.NewLineItem("A", 2, usd)
	b, _ := order.NewLineItem("B", 1, usd)

	o, err := order.New(order.Params{ID: "o-1", CustomerID: "c-1", Items: []order.LineItem{a, b}, Now: now})
	if err != nil {
		t.Fatal(err)
	}

	evs := o.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("after New: %d events, want 1", len(evs))
	}
	created, ok := evs[0].(order.Created)
	if !ok {
		t.Fatalf("event = %T, want order.Created", evs[0])
	}
	if created.OrderID() != "o-1" || created.AggregateID() != "o-1" || created.CustomerID() != "c-1" || !created.OccurredAt().Equal(now) {
		t.Fatalf("created = %+v", created)
	}
	if created.TotalAmount() != 750 || created.TotalCurrency() != "USD" {
		t.Fatalf("total = %d %s", created.TotalAmount(), created.TotalCurrency())
	}
	items := created.Items()
	if len(items) != 2 || items[0].SKU() != "A" || items[0].Quantity() != 2 || items[1] != order.NewCreatedItem("B", 1) {
		t.Fatalf("items = %+v", items)
	}
	items[0] = order.NewCreatedItem("Z", 9) // copy: must not alias the event
	if created.Items()[0].SKU() != "A" {
		t.Fatal("Items() aliased the event's slice")
	}

	if got := o.PullEvents(); len(got) != 0 {
		t.Fatalf("second pull: %d events, want 0", len(got))
	}

	confirmedAt := now.Add(30 * time.Second)
	if err := o.Confirm(confirmedAt); err != nil {
		t.Fatal(err)
	}
	evs = o.PullEvents()
	confirmed, ok := evs[0].(order.Confirmed)
	if len(evs) != 1 || !ok || confirmed.OrderID() != "o-1" || confirmed.CustomerID() != "c-1" || !confirmed.OccurredAt().Equal(confirmedAt) {
		t.Fatalf("after Confirm: %+v", evs)
	}

	// Completed is raised by Complete; try it on a copy so the cancel path below still runs.
	done, _ := order.Reconstitute(order.ReconstituteParams{ID: "o-2", CustomerID: "c-2", Items: []order.LineItem{a}, Status: order.StatusConfirmed, CreatedAt: now, UpdatedAt: now})
	completedAt := now.Add(45 * time.Second)
	if err := done.Complete(completedAt); err != nil {
		t.Fatal(err)
	}
	evs = done.PullEvents()
	completed, ok := evs[0].(order.Completed)
	if len(evs) != 1 || !ok || completed.OrderID() != "o-2" || completed.CustomerID() != "c-2" || !completed.OccurredAt().Equal(completedAt) {
		t.Fatalf("after Complete: %+v", evs)
	}

	later := now.Add(time.Minute)
	if err := o.Cancel(later); err != nil {
		t.Fatal(err)
	}
	evs = o.PullEvents()
	cancelled, ok := evs[0].(order.Cancelled)
	if len(evs) != 1 || !ok || cancelled.OrderID() != "o-1" || cancelled.CustomerID() != "c-1" || !cancelled.OccurredAt().Equal(later) {
		t.Fatalf("after Cancel: %+v", evs)
	}

	// A failed transition raises nothing.
	if err := o.Cancel(later); err == nil {
		t.Fatal("second cancel should fail")
	}
	if got := o.PullEvents(); len(got) != 0 {
		t.Fatalf("failed cancel raised %d events", len(got))
	}

	// Reconstitute never replays history as events.
	r, err := order.Reconstitute(order.ReconstituteParams{ID: "o-1", CustomerID: "c-1", Items: []order.LineItem{a}, Status: order.StatusPending, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.PullEvents(); len(got) != 0 {
		t.Fatalf("reconstituted order has %d events", len(got))
	}
}
