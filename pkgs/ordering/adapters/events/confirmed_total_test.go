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

// OrderConfirmed.total is the order's real total (docs/payment.md §2): Payment bills
// from it, so a missing or zero total must never reach the wire. Two lines with
// different unit prices make the total (620) differ from every unit price and from
// either line's subtotal, so a field filled from the wrong source cannot pass.
func TestPublisher_OrderConfirmedCarriesOrderTotal(t *testing.T) {
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	usd := func(n int64) kernel.Money { m, _ := kernel.NewMoney(n, "USD"); return m }
	line := func(sku string, qty int32, price int64) order.LineItem {
		li, err := order.NewLineItem(sku, qty, usd(price))
		if err != nil {
			t.Fatal(err)
		}
		return li
	}
	o, err := order.New(order.Params{ID: "o-total", CustomerID: "c-1", Items: []order.LineItem{line("A", 2, 250), line("B", 1, 120)}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if o.Total().Amount() != 620 {
		t.Fatalf("fixture total = %d, want 620", o.Total().Amount())
	}
	if err := o.Confirm(now.Add(time.Second), false); err != nil {
		t.Fatal(err)
	}

	sink := &capture{}
	if err := events.NewPublisher(sink).Publish(context.Background(), o.PullEvents()...); err != nil {
		t.Fatal(err)
	}
	var confirmed *orderingv1.OrderConfirmed
	for i, topic := range sink.topics {
		if topic != events.TopicOrderConfirmed {
			continue
		}
		confirmed = &orderingv1.OrderConfirmed{}
		if err := proto.Unmarshal(sink.msgs[i].Payload, confirmed); err != nil {
			t.Fatal(err)
		}
	}
	if confirmed == nil {
		t.Fatalf("no OrderConfirmed published: %v", sink.topics)
	}

	// Reflection, so the file compiles against a contract that has no GetTotal yet.
	msg := confirmed.ProtoReflect()
	f := msg.Descriptor().Fields().ByName("total")
	if f == nil {
		t.Fatal("OrderConfirmed has no total field")
	}
	if !msg.Has(f) {
		t.Fatal("OrderConfirmed.total is unset: a consumer would read it as a 0 charge")
	}
	money := msg.Get(f).Message()
	amount := money.Get(money.Descriptor().Fields().ByName("amount_minor")).Int()
	currency := money.Get(money.Descriptor().Fields().ByName("currency")).String()
	if amount != 620 || currency != "USD" {
		t.Fatalf("OrderConfirmed.total = %d %s, want 620 USD", amount, currency)
	}
}
