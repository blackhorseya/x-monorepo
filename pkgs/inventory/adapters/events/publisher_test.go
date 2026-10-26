package events_test

import (
	"context"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"google.golang.org/protobuf/proto"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/adapters/events"
	inventoryv1 "github.com/blackhorseya/x-monorepo/pkgs/inventory/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/stock"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
)

type capture struct {
	topics []string
	msgs   []*message.Message
}

func (c *capture) Publish(topic string, msgs ...*message.Message) error {
	for _, m := range msgs {
		c.topics = append(c.topics, topic)
		c.msgs = append(c.msgs, m)
	}
	return nil
}

func (c *capture) Close() error { return nil }

func TestPublisher_MapsDomainEventsToWire(t *testing.T) {
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	a, _ := stock.New("A", 5, now)
	line := func(sku string, qty int32) reservation.Line {
		l, err := reservation.NewLine(sku, qty)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}

	ok, err := reservation.Reserve(reservation.Params{OrderID: "o-1", Lines: []reservation.Line{line("A", 2)}, Now: now}, map[string]*stock.Stock{"A": a})
	if err != nil {
		t.Fatal(err)
	}
	if err := ok.Release(map[string]*stock.Stock{"A": a}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	short, err := reservation.Reserve(reservation.Params{OrderID: "o-2", Lines: []reservation.Line{line("A", 9), line("Z", 1)}, Now: now}, map[string]*stock.Stock{"A": a})
	if err != nil {
		t.Fatal(err)
	}

	sink := &capture{}
	pub := events.NewPublisher(sink)
	if err := pub.Publish(context.Background(), ok.PullEvents()...); err != nil {
		t.Fatal(err)
	}
	if err := pub.Publish(context.Background(), short.PullEvents()...); err != nil {
		t.Fatal(err)
	}
	want := []string{"inventory.v1.StockReserved", "inventory.v1.StockReleased", "ordering.v1.OrderCancellationRequested"}
	if len(sink.topics) != 3 || sink.topics[0] != want[0] || sink.topics[1] != want[1] || sink.topics[2] != want[2] {
		t.Fatalf("topics = %v, want %v", sink.topics, want)
	}
	if eventbus.TableName(sink.topics[2]) != "ordering.events_v1_ordercancellationrequested" {
		t.Fatal("a request to ordering must land in ordering's table")
	}

	var reserved inventoryv1.StockReserved
	if err := proto.Unmarshal(sink.msgs[0].Payload, &reserved); err != nil {
		t.Fatal(err)
	}
	if reserved.GetOrderId() != "o-1" || len(reserved.GetLines()) != 1 || reserved.GetLines()[0].GetQuantity() != 2 || !reserved.GetReservedAt().AsTime().Equal(now) {
		t.Fatalf("StockReserved = %v", &reserved)
	}
	if sink.msgs[0].Metadata.Get(eventbus.MetaKey) != "o-1" || sink.msgs[0].Metadata.Get(eventbus.MetaOccurredAt) != "2026-10-18T09:00:00Z" {
		t.Fatalf("metadata = %v", sink.msgs[0].Metadata)
	}

	var released inventoryv1.StockReleased
	if err := proto.Unmarshal(sink.msgs[1].Payload, &released); err != nil {
		t.Fatal(err)
	}
	if released.GetOrderId() != "o-1" || !released.GetReleasedAt().AsTime().Equal(now.Add(time.Minute)) {
		t.Fatalf("StockReleased = %v", &released)
	}

	// Committed, on a third order.
	sold, err := reservation.Reserve(reservation.Params{OrderID: "o-3", Lines: []reservation.Line{line("A", 1)}, Now: now}, map[string]*stock.Stock{"A": a})
	if err != nil {
		t.Fatal(err)
	}
	if err := sold.Commit(map[string]*stock.Stock{"A": a}, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := pub.Publish(context.Background(), sold.PullEvents()...); err != nil {
		t.Fatal(err)
	}
	if len(sink.topics) != 5 || sink.topics[4] != "inventory.v1.StockCommitted" || sink.msgs[4].Metadata.Get(eventbus.MetaKey) != "o-3" {
		t.Fatalf("topics = %v", sink.topics)
	}
	var committed inventoryv1.StockCommitted
	if err := proto.Unmarshal(sink.msgs[4].Payload, &committed); err != nil {
		t.Fatal(err)
	}
	if committed.GetOrderId() != "o-3" || !committed.GetCommittedAt().AsTime().Equal(now.Add(2*time.Minute)) {
		t.Fatalf("StockCommitted = %v", &committed)
	}

	// Expired, on a fourth order: Released then the cancellation request with the hold in the reason.
	stale, err := reservation.Reserve(reservation.Params{OrderID: "o-4", Lines: []reservation.Line{line("A", 1)}, Now: now}, map[string]*stock.Stock{"A": a})
	if err != nil {
		t.Fatal(err)
	}
	if err := stale.Expire(map[string]*stock.Stock{"A": a}, now.Add(25*time.Hour+30*time.Second+500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := pub.Publish(context.Background(), stale.PullEvents()...); err != nil {
		t.Fatal(err)
	}
	// Reserved (from Reserve) then Released then the request.
	if len(sink.topics) != 8 || sink.topics[6] != "inventory.v1.StockReleased" || sink.topics[7] != "ordering.v1.OrderCancellationRequested" {
		t.Fatalf("topics = %v", sink.topics)
	}
	var expired orderingv1.OrderCancellationRequested
	if err := proto.Unmarshal(sink.msgs[7].Payload, &expired); err != nil {
		t.Fatal(err)
	}
	if expired.GetOrderId() != "o-4" || expired.GetReason() != "inventory: reservation expired after 25h0m30s" {
		t.Fatalf("expired request = %v", &expired)
	}

	var req orderingv1.OrderCancellationRequested
	if err := proto.Unmarshal(sink.msgs[2].Payload, &req); err != nil {
		t.Fatal(err)
	}
	if req.GetOrderId() != "o-2" || req.GetReason() != "inventory: insufficient stock for A, Z" || !req.GetRequestedAt().AsTime().Equal(now) {
		t.Fatalf("OrderCancellationRequested = %v", &req)
	}
}

type unknownEvent struct{ reservation.Event }

func TestPublisher_UnknownEvent(t *testing.T) {
	if err := events.NewPublisher(&capture{}).Publish(context.Background(), unknownEvent{}); err == nil {
		t.Fatal("unmapped event must fail loudly")
	}
}
