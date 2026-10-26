// Package events is the outbound adapter for domain events: it maps
// order.Event values to the ordering.v1 wire contract and hands them to a
// Watermill publisher. Like adapters/rpc it is one of the few packages that
// import contract/; the domain and the use cases never see proto.
package events

import (
	"context"
	"fmt"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/port"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	commonv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/common/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
)

// Topics published by ordering: the proto full names.
var (
	TopicOrderCreated   = string((&orderingv1.OrderCreated{}).ProtoReflect().Descriptor().FullName())
	TopicOrderCancelled = string((&orderingv1.OrderCancelled{}).ProtoReflect().Descriptor().FullName())
	TopicOrderConfirmed = string((&orderingv1.OrderConfirmed{}).ProtoReflect().Descriptor().FullName())
)

var _ port.EventPublisher = (*Publisher)(nil)

// Publisher implements port.EventPublisher on a message.Publisher. With the
// Postgres publisher the rows join the caller's transaction (the context must
// come from Transactor.Do); with GoChannel they are delivered in-process.
type Publisher struct {
	pub message.Publisher
}

func NewPublisher(pub message.Publisher) *Publisher { return &Publisher{pub: pub} }

func (h *Publisher) Publish(c context.Context, events ...order.Event) error {
	for _, e := range events {
		topic, pb, err := toProto(e)
		if err != nil {
			return err
		}
		payload, err := proto.Marshal(pb)
		if err != nil {
			return fmt.Errorf("encode %s: %w", topic, err)
		}

		msg := message.NewMessageWithContext(c, watermill.NewUUID(), payload)
		msg.Metadata.Set(eventbus.MetaKey, e.AggregateID())
		msg.Metadata.Set(eventbus.MetaOccurredAt, e.OccurredAt().UTC().Format("2006-01-02T15:04:05.999999999Z07:00"))
		if err := h.pub.Publish(topic, msg); err != nil {
			return fmt.Errorf("publish %s for order %s: %w", topic, e.OrderID(), err)
		}
	}

	return nil
}

func toProto(e order.Event) (string, proto.Message, error) {
	switch ev := e.(type) {
	case order.Created:
		items := make([]*orderingv1.OrderCreatedItem, 0, len(ev.Items()))
		for _, it := range ev.Items() {
			items = append(items, &orderingv1.OrderCreatedItem{Sku: it.SKU(), Quantity: it.Quantity()})
		}

		return TopicOrderCreated, &orderingv1.OrderCreated{
			OrderId:    ev.OrderID().String(),
			CustomerId: ev.CustomerID(),
			Items:      items,
			Total:      &commonv1.Money{AmountMinor: ev.TotalAmount(), Currency: ev.TotalCurrency()},
			CreatedAt:  timestamppb.New(ev.OccurredAt()),
		}, nil
	case order.Confirmed:
		return TopicOrderConfirmed, &orderingv1.OrderConfirmed{
			OrderId:     ev.OrderID().String(),
			CustomerId:  ev.CustomerID(),
			ConfirmedAt: timestamppb.New(ev.OccurredAt()),
		}, nil
	case order.Cancelled:
		return TopicOrderCancelled, &orderingv1.OrderCancelled{
			OrderId:     ev.OrderID().String(),
			CustomerId:  ev.CustomerID(),
			CancelledAt: timestamppb.New(ev.OccurredAt()),
		}, nil
	default:
		return "", nil, fmt.Errorf("no wire mapping for domain event %T", e)
	}
}
