// Package consumer adapts ordering's integration events to inventory use
// cases: OrderCreated → ReserveStock, OrderCancelled → ReleaseStock. It
// decodes ordering's contract (the only ordering package a BC may import)
// and knows nothing about subscription, ack or retry — that is
// platform/eventbus (Watermill router) and apps/worker.
package consumer

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// Topics this BC subscribes to: ordering's proto full names.
var (
	TopicOrderCreated   = string((&orderingv1.OrderCreated{}).ProtoReflect().Descriptor().FullName())
	TopicOrderCancelled = string((&orderingv1.OrderCancelled{}).ProtoReflect().Descriptor().FullName())
)

// ErrDrop tells the router the message is poison or no longer applicable and
// must be acknowledged without retry (eventbus.ErrDrop).
var ErrDrop = eventbus.ErrDrop

type (
	ReserveStockUC = usecasex.UseCase[dto.ReserveStockInput, *dto.ReservationOutput]
	ReleaseStockUC = usecasex.UseCase[dto.ReleaseStockInput, *dto.ReservationOutput]
)

// OrderCreatedHandler reserves stock for every new order.
type OrderCreatedHandler struct {
	reserve ReserveStockUC
}

func NewOrderCreatedHandler(reserve ReserveStockUC) *OrderCreatedHandler {
	return &OrderCreatedHandler{reserve: reserve}
}

// Handle decodes the payload and runs the use case.
//
// Error contract (eventbus):
//   - nil            → ack
//   - ErrDrop        → ack and drop (undecodable / permanently invalid)
//   - any other err  → retry, then poison queue
func (h *OrderCreatedHandler) Handle(c context.Context, payload []byte) error {
	ctx := contextx.FromContext(c)

	var evt orderingv1.OrderCreated
	if err := proto.Unmarshal(payload, &evt); err != nil {
		ctx.Warn("undecodable event", "topic", TopicOrderCreated, "error", err)
		return fmt.Errorf("%w: %w", ErrDrop, err)
	}

	lines := make([]dto.Line, 0, len(evt.GetItems()))
	for _, it := range evt.GetItems() {
		lines = append(lines, dto.Line{SKU: it.GetSku(), Quantity: it.GetQuantity()})
	}

	out, err := h.reserve.Execute(ctx, dto.ReserveStockInput{OrderID: evt.GetOrderId(), Lines: lines})
	if err != nil {
		return final(ctx, TopicOrderCreated, evt.GetOrderId(), err)
	}
	if len(out.ShortfallSKUs) > 0 {
		ctx.Info("stock short, cancellation requested", "topic", TopicOrderCreated, "order_id", out.OrderID, "skus", out.ShortfallSKUs)
	}

	return nil
}

// OrderCancelledHandler releases what a cancelled order had reserved.
type OrderCancelledHandler struct {
	release ReleaseStockUC
}

func NewOrderCancelledHandler(release ReleaseStockUC) *OrderCancelledHandler {
	return &OrderCancelledHandler{release: release}
}

// Handle follows the same error contract as OrderCreatedHandler.Handle.
func (h *OrderCancelledHandler) Handle(c context.Context, payload []byte) error {
	ctx := contextx.FromContext(c)

	var evt orderingv1.OrderCancelled
	if err := proto.Unmarshal(payload, &evt); err != nil {
		ctx.Warn("undecodable event", "topic", TopicOrderCancelled, "error", err)
		return fmt.Errorf("%w: %w", ErrDrop, err)
	}

	if _, err := h.release.Execute(ctx, dto.ReleaseStockInput{OrderID: evt.GetOrderId()}); err != nil {
		return final(ctx, TopicOrderCancelled, evt.GetOrderId(), err)
	}

	return nil
}

// final decides between drop and retry. Domain rejections (kernel.Error:
// unknown reservation, already released, rejected) are final: retrying won't
// change the aggregate's answer. Everything else — including stock.ErrStale,
// a lost optimistic-lock race — is infrastructure and is retried.
func final(ctx contextx.Contextx, topic, orderID string, err error) error {
	var de *kernel.Error
	if errors.As(err, &de) {
		ctx.Info("event rejected by domain", "topic", topic, "order_id", orderID, "code", de.Code())
		return fmt.Errorf("%w: %w", ErrDrop, err)
	}

	return err
}
