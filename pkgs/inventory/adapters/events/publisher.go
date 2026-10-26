// Package events is the outbound adapter for domain events: it maps
// reservation.Event values to the wire contract and hands them to a Watermill
// publisher. Reserved / Released / Committed are inventory.v1 messages; Rejected
// and Expired become ordering.v1.OrderCancellationRequested — a request owned by
// ordering's contract, published into ordering's topic (and table) so ordering's
// consumer picks it up.
package events

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/port"
	inventoryv1 "github.com/blackhorseya/x-monorepo/pkgs/inventory/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
)

// Topics published by inventory: the proto full names.
var (
	TopicStockReserved              = string((&inventoryv1.StockReserved{}).ProtoReflect().Descriptor().FullName())
	TopicStockReleased              = string((&inventoryv1.StockReleased{}).ProtoReflect().Descriptor().FullName())
	TopicStockCommitted             = string((&inventoryv1.StockCommitted{}).ProtoReflect().Descriptor().FullName())
	TopicOrderCancellationRequested = string((&orderingv1.OrderCancellationRequested{}).ProtoReflect().Descriptor().FullName())
)

var _ port.EventPublisher = (*Publisher)(nil)

// Publisher implements port.EventPublisher on a message.Publisher. With the
// Postgres publisher the rows join the caller's transaction (the context must
// come from Transactor.Do); with GoChannel they are delivered in-process.
type Publisher struct {
	pub message.Publisher
}

func NewPublisher(pub message.Publisher) *Publisher { return &Publisher{pub: pub} }

func (h *Publisher) Publish(c context.Context, events ...reservation.Event) error {
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

func toProto(e reservation.Event) (string, proto.Message, error) {
	switch ev := e.(type) {
	case reservation.Reserved:
		lines := make([]*inventoryv1.StockReservedLine, 0, len(ev.Lines()))
		for _, l := range ev.Lines() {
			lines = append(lines, &inventoryv1.StockReservedLine{Sku: l.SKU(), Quantity: l.Quantity()})
		}

		return TopicStockReserved, &inventoryv1.StockReserved{
			OrderId:    ev.OrderID(),
			Lines:      lines,
			ReservedAt: timestamppb.New(ev.OccurredAt()),
		}, nil
	case reservation.Rejected:
		return TopicOrderCancellationRequested, &orderingv1.OrderCancellationRequested{
			OrderId:     ev.OrderID(),
			Reason:      "inventory: insufficient stock for " + strings.Join(ev.Shortfall(), ", "),
			RequestedAt: timestamppb.New(ev.OccurredAt()),
		}, nil
	case reservation.Expired:
		return TopicOrderCancellationRequested, &orderingv1.OrderCancellationRequested{
			OrderId:     ev.OrderID(),
			Reason:      "inventory: reservation expired after " + ev.HeldFor().Truncate(time.Second).String(),
			RequestedAt: timestamppb.New(ev.OccurredAt()),
		}, nil
	case reservation.Released:
		return TopicStockReleased, &inventoryv1.StockReleased{
			OrderId:    ev.OrderID(),
			ReleasedAt: timestamppb.New(ev.OccurredAt()),
		}, nil
	case reservation.Committed:
		return TopicStockCommitted, &inventoryv1.StockCommitted{
			OrderId:     ev.OrderID(),
			CommittedAt: timestamppb.New(ev.OccurredAt()),
		}, nil
	default:
		return "", nil, fmt.Errorf("no wire mapping for domain event %T", e)
	}
}
