// Package consumer adapts broker messages to use cases. It drives the SAME
// CancelOrder use case as the rpc adapter; only the decode step differs.
// Broker plumbing (NATS / SQS subscription, ack, retry) belongs to the app or a
// platform package; this handler only knows "bytes in, error out".
package consumer

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// Topic is the subject this handler subscribes to.
const Topic = "ordering.v1.OrderCancellationRequested"

// ErrDrop tells the broker plumbing the message is poison and must not be retried.
var ErrDrop = errors.New("consumer: drop message")

type CancelOrderUC = usecasex.UseCase[dto.CancelOrderInput, *dto.OrderOutput]

// OrderCancellationHandler consumes OrderCancellationRequested events.
type OrderCancellationHandler struct {
	cancelOrder CancelOrderUC
}

func NewOrderCancellationHandler(cancelOrder CancelOrderUC) *OrderCancellationHandler {
	return &OrderCancellationHandler{cancelOrder: cancelOrder}
}

// Handle decodes the payload and runs the use case.
//
// Error contract for the broker plumbing:
//   - nil            → ack
//   - ErrDrop        → ack and drop (undecodable / permanently invalid)
//   - any other err  → nack / retry
func (h *OrderCancellationHandler) Handle(c context.Context, payload []byte) error {
	ctx := contextx.FromContext(c)

	var evt orderingv1.OrderCancellationRequested
	if err := proto.Unmarshal(payload, &evt); err != nil {
		ctx.Warn("undecodable event", "topic", Topic, "error", err)
		return fmt.Errorf("%w: %w", ErrDrop, err)
	}

	_, err := h.cancelOrder.Execute(ctx, dto.CancelOrderInput{
		OrderID: evt.GetOrderId(),
		Reason:  evt.GetReason(),
	})
	if err == nil {
		return nil
	}

	// Domain rejections are final: retrying won't change the aggregate's answer.
	var de *kernel.Error
	if errors.As(err, &de) {
		ctx.Info("event rejected by domain", "topic", Topic, "order_id", evt.GetOrderId(), "code", de.Code())
		return fmt.Errorf("%w: %w", ErrDrop, err)
	}

	return err // infrastructure error → retry
}
