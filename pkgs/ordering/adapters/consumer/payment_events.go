package consumer

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	paymentv1 "github.com/blackhorseya/x-monorepo/pkgs/payment/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// Payment's events this BC consumes. Only payment's contract/ is imported (the consumer of an event
// owns nothing of the producer but its wire format); the outbox tables belong to payment's
// migrations, this BC only puts a consumer-group cursor on them.
var (
	TopicPaymentCaptured = string((&paymentv1.PaymentCaptured{}).ProtoReflect().Descriptor().FullName())
	TopicPaymentExpired  = string((&paymentv1.PaymentExpired{}).ProtoReflect().Descriptor().FullName())
)

type (
	ApplyPaymentCapturedUC = usecasex.UseCase[dto.PaymentCapturedInput, *dto.PaymentEventOutput]
	ApplyPaymentExpiredUC  = usecasex.UseCase[dto.PaymentExpiredInput, *dto.PaymentEventOutput]
)

// PaymentCapturedHandler consumes payment.v1.PaymentCaptured.
type PaymentCapturedHandler struct {
	apply ApplyPaymentCapturedUC
}

func NewPaymentCapturedHandler(apply ApplyPaymentCapturedUC) *PaymentCapturedHandler {
	return &PaymentCapturedHandler{apply: apply}
}

// Handle follows the error contract of OrderCancellationHandler.Handle: a domain rejection (a
// cancelled order, an order never confirmed, an unknown order) is final → drop; payment decides
// the refund on its own side. Only an infrastructure error (incl. order.ErrStale) retries.
func (h *PaymentCapturedHandler) Handle(c context.Context, payload []byte) error {
	ctx := contextx.FromContext(c)

	var evt paymentv1.PaymentCaptured
	if err := proto.Unmarshal(payload, &evt); err != nil {
		ctx.Warn("undecodable event", "topic", TopicPaymentCaptured, "error", err)
		return fmt.Errorf("%w: %w", ErrDrop, err)
	}

	_, err := h.apply.Execute(ctx, dto.PaymentCapturedInput{OrderID: evt.GetOrderId()})

	return dropDomain(ctx, TopicPaymentCaptured, evt.GetOrderId(), err)
}

// PaymentExpiredHandler consumes payment.v1.PaymentExpired.
type PaymentExpiredHandler struct {
	apply ApplyPaymentExpiredUC
}

func NewPaymentExpiredHandler(apply ApplyPaymentExpiredUC) *PaymentExpiredHandler {
	return &PaymentExpiredHandler{apply: apply}
}

// Handle follows the same error contract as PaymentCapturedHandler.Handle.
func (h *PaymentExpiredHandler) Handle(c context.Context, payload []byte) error {
	ctx := contextx.FromContext(c)

	var evt paymentv1.PaymentExpired
	if err := proto.Unmarshal(payload, &evt); err != nil {
		ctx.Warn("undecodable event", "topic", TopicPaymentExpired, "error", err)
		return fmt.Errorf("%w: %w", ErrDrop, err)
	}

	_, err := h.apply.Execute(ctx, dto.PaymentExpiredInput{OrderID: evt.GetOrderId()})

	return dropDomain(ctx, TopicPaymentExpired, evt.GetOrderId(), err)
}

// dropDomain turns a domain rejection into ErrDrop and leaves everything else to retry.
func dropDomain(ctx contextx.Contextx, topic, orderID string, err error) error {
	if err == nil {
		return nil
	}
	var de *kernel.Error
	if errors.As(err, &de) {
		ctx.Info("event rejected by domain", "topic", topic, "order_id", orderID, "code", de.Code())
		return fmt.Errorf("%w: %w", ErrDrop, err)
	}

	return err // infrastructure error → retry
}
