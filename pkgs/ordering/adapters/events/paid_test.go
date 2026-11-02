package events_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/events"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
)

// MarkPaid's event reaches the wire as ordering.v1.OrderPaid, keyed by the order id.
func TestPublisher_OrderPaid(t *testing.T) {
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	usd, _ := kernel.NewMoney(250, "USD")
	li, _ := order.NewLineItem("SKU-1", 2, usd)
	o, err := order.Reconstitute(order.ReconstituteParams{ID: "o-paid", CustomerID: "c-9", Items: []order.LineItem{li}, Status: order.StatusConfirmed, PaymentRequired: true, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	paidAt := now.Add(time.Minute)
	if err := o.MarkPaid(paidAt); err != nil {
		t.Fatal(err)
	}
	sink := &capture{}
	if err := events.NewPublisher(sink).Publish(context.Background(), o.PullEvents()...); err != nil {
		t.Fatal(err)
	}
	if len(sink.msgs) != 1 || sink.topics[0] != "ordering.v1.OrderPaid" || sink.topics[0] != events.TopicOrderPaid || sink.msgs[0].Metadata.Get("key") != "o-paid" {
		t.Fatalf("topics = %v", sink.topics)
	}
	var paid orderingv1.OrderPaid
	if err := proto.Unmarshal(sink.msgs[0].Payload, &paid); err != nil {
		t.Fatal(err)
	}
	if paid.GetOrderId() != "o-paid" || paid.GetCustomerId() != "c-9" || !paid.GetPaidAt().AsTime().Equal(paidAt) {
		t.Fatalf("OrderPaid = %v", &paid)
	}
}
