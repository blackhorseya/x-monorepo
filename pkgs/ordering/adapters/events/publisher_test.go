package events_test

import (
	"context"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"google.golang.org/protobuf/proto"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/events"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
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
	usd, _ := kernel.NewMoney(250, "USD")
	li, _ := order.NewLineItem("SKU-1", 2, usd)
	o, err := order.New(order.Params{ID: "o-1", CustomerID: "c-1", Items: []order.LineItem{li}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Confirm(now.Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := o.Cancel(now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	sink := &capture{}
	if err := events.NewPublisher(sink).Publish(context.Background(), o.PullEvents()...); err != nil {
		t.Fatal(err)
	}
	if len(sink.msgs) != 3 || sink.topics[0] != "ordering.v1.OrderCreated" || sink.topics[1] != "ordering.v1.OrderConfirmed" || sink.topics[2] != "ordering.v1.OrderCancelled" {
		t.Fatalf("topics = %v", sink.topics)
	}

	var created orderingv1.OrderCreated
	if err := proto.Unmarshal(sink.msgs[0].Payload, &created); err != nil {
		t.Fatal(err)
	}
	if created.GetOrderId() != "o-1" || created.GetCustomerId() != "c-1" || created.GetTotal().GetAmountMinor() != 500 ||
		len(created.GetItems()) != 1 || created.GetItems()[0].GetSku() != "SKU-1" || created.GetItems()[0].GetQuantity() != 2 ||
		!created.GetCreatedAt().AsTime().Equal(now) {
		t.Fatalf("OrderCreated = %v", &created)
	}
	if sink.msgs[0].Metadata.Get(eventbus.MetaKey) != "o-1" || sink.msgs[0].Metadata.Get(eventbus.MetaOccurredAt) != "2026-10-18T09:00:00Z" {
		t.Fatalf("metadata = %v", sink.msgs[0].Metadata)
	}

	var confirmed orderingv1.OrderConfirmed
	if err := proto.Unmarshal(sink.msgs[1].Payload, &confirmed); err != nil {
		t.Fatal(err)
	}
	if confirmed.GetOrderId() != "o-1" || confirmed.GetCustomerId() != "c-1" || !confirmed.GetConfirmedAt().AsTime().Equal(now.Add(30*time.Second)) {
		t.Fatalf("OrderConfirmed = %v", &confirmed)
	}

	var cancelled orderingv1.OrderCancelled
	if err := proto.Unmarshal(sink.msgs[2].Payload, &cancelled); err != nil {
		t.Fatal(err)
	}
	if cancelled.GetOrderId() != "o-1" || !cancelled.GetCancelledAt().AsTime().Equal(now.Add(time.Minute)) {
		t.Fatalf("OrderCancelled = %v", &cancelled)
	}
}
