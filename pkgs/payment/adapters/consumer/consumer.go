// Package consumer adapts messages from the event bus to payment's use cases: "bytes in, error out".
// Subscription, ack, retry and the poison queue are platform/eventbus (the Watermill router) and the
// composition root; the handlers here only decode and drive a use case.
//
// The error contract (eventbus): nil acks; a domain rejection (kernel.Error) is final and dropped;
// anything else (infrastructure, a lost optimistic lock, the provider not answering) is retried with
// backoff and finally parked on payment.poison. Nothing the provider said is ever in an error here.
package consumer

import (
	"context"

	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	paymentv1 "github.com/blackhorseya/x-monorepo/pkgs/payment/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// Topics this BC consumes: the proto full names. Ordering's three belong to ordering's schema (only
// its contract/ is imported; payment puts a consumer-group cursor on its tables); the other two are
// payment's own work items.
var (
	TopicOrderConfirmed         = string((&orderingv1.OrderConfirmed{}).ProtoReflect().Descriptor().FullName())
	TopicOrderCancelled         = string((&orderingv1.OrderCancelled{}).ProtoReflect().Descriptor().FullName())
	TopicOrderCompleted         = string((&orderingv1.OrderCompleted{}).ProtoReflect().Descriptor().FullName())
	TopicPaymentRefundRequested = string((&paymentv1.PaymentRefundRequested{}).ProtoReflect().Descriptor().FullName())
)

// ErrDrop tells the router the message is final: acknowledge it, do not retry.
var ErrDrop = eventbus.ErrDrop

type (
	OrderConfirmedUC = usecasex.UseCase[dto.OrderConfirmedInput, *dto.Ack]
	RefundUC         = usecasex.UseCase[dto.RefundInput, *dto.Ack]
)

// OrderCancelledUC and OrderCompletedUC have the same shape and are different use cases: distinct
// types, so wire can tell which one a handler is given.
type (
	OrderCancelledUC usecasex.UseCase[dto.OrderEndedInput, *dto.OrderEndedOutput]
	OrderCompletedUC usecasex.UseCase[dto.OrderEndedInput, *dto.OrderEndedOutput]
)

// SPEC STAGE: the handlers acknowledge everything and drive nothing.

type OrderConfirmedHandler struct{ uc OrderConfirmedUC }

func NewOrderConfirmedHandler(uc OrderConfirmedUC) *OrderConfirmedHandler {
	return &OrderConfirmedHandler{uc: uc}
}

func (h *OrderConfirmedHandler) Handle(context.Context, []byte) error { return nil }

type OrderCancelledHandler struct{ uc OrderCancelledUC }

func NewOrderCancelledHandler(uc OrderCancelledUC) *OrderCancelledHandler {
	return &OrderCancelledHandler{uc: uc}
}

func (h *OrderCancelledHandler) Handle(context.Context, []byte) error { return nil }

type OrderCompletedHandler struct{ uc OrderCompletedUC }

func NewOrderCompletedHandler(uc OrderCompletedUC) *OrderCompletedHandler {
	return &OrderCompletedHandler{uc: uc}
}

func (h *OrderCompletedHandler) Handle(context.Context, []byte) error { return nil }

type RefundHandler struct{ uc RefundUC }

func NewRefundHandler(uc RefundUC) *RefundHandler { return &RefundHandler{uc: uc} }

func (h *RefundHandler) Handle(context.Context, []byte) error { return nil }
