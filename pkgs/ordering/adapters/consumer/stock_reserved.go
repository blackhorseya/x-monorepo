package consumer

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	inventoryv1 "github.com/blackhorseya/x-monorepo/pkgs/inventory/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// TopicStockReserved is inventory's event this BC consumes to confirm an order.
// Only inventory's contract/ is imported (the consumer of an event owns nothing
// of the producer but its wire format); the outbox table belongs to inventory's
// migrations, this BC only puts a consumer-group cursor on it.
var TopicStockReserved = string((&inventoryv1.StockReserved{}).ProtoReflect().Descriptor().FullName())

type ConfirmOrderUC = usecasex.UseCase[dto.ConfirmOrderInput, *dto.OrderOutput]

// StockReservedHandler consumes inventory.v1.StockReserved: every line of the
// order was reserved, so the order moves PENDING → CONFIRMED.
type StockReservedHandler struct {
	confirmOrder ConfirmOrderUC
}

func NewStockReservedHandler(confirmOrder ConfirmOrderUC) *StockReservedHandler {
	return &StockReservedHandler{confirmOrder: confirmOrder}
}

// Handle follows the same error contract as OrderCancellationHandler.Handle.
// A StockReserved for an order already cancelled (the customer cancelled while
// inventory was reserving) is a domain rejection → drop; inventory releases the
// reservation on the OrderCancelled it receives for the same order.
func (h *StockReservedHandler) Handle(c context.Context, payload []byte) error {
	ctx := contextx.FromContext(c)

	var evt inventoryv1.StockReserved
	if err := proto.Unmarshal(payload, &evt); err != nil {
		ctx.Warn("undecodable event", "topic", TopicStockReserved, "error", err)
		return fmt.Errorf("%w: %w", ErrDrop, err)
	}

	_, err := h.confirmOrder.Execute(ctx, dto.ConfirmOrderInput{OrderID: evt.GetOrderId()})
	if err == nil {
		return nil
	}

	var de *kernel.Error
	if errors.As(err, &de) {
		ctx.Info("event rejected by domain", "topic", TopicStockReserved, "order_id", evt.GetOrderId(), "code", de.Code())
		return fmt.Errorf("%w: %w", ErrDrop, err)
	}

	return err // infrastructure error (incl. order.ErrStale) → retry
}
