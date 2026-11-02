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
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	paymentv1 "github.com/blackhorseya/x-monorepo/pkgs/payment/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
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
	TopicWebhookReceived        = string((&paymentv1.WebhookReceived{}).ProtoReflect().Descriptor().FullName())
)

// ErrDrop tells the router the message is final: acknowledge it, do not retry.
var ErrDrop = eventbus.ErrDrop

type (
	OrderConfirmedUC = usecasex.UseCase[dto.OrderConfirmedInput, *dto.Ack]
	RefundUC         = usecasex.UseCase[dto.RefundInput, *dto.Ack]
	WebhookUC        = usecasex.UseCase[dto.WebhookReceivedInput, *dto.Ack]
)

// OrderCancelledUC and OrderCompletedUC have the same shape and are different use cases: distinct
// types, so wire can tell which one a handler is given.
type (
	OrderCancelledUC usecasex.UseCase[dto.OrderEndedInput, *dto.OrderEndedOutput]
	OrderCompletedUC usecasex.UseCase[dto.OrderEndedInput, *dto.OrderEndedOutput]
)

// decode unmarshals payload into msg; an undecodable message is final.
func decode(ctx contextx.Contextx, topic string, payload []byte, msg proto.Message) error {
	if err := proto.Unmarshal(payload, msg); err != nil {
		ctx.Warn("undecodable event", "topic", topic)

		return fmt.Errorf("%w: undecodable %s", ErrDrop, topic)
	}

	return nil
}

// result turns a use case's error into the router's contract. key names what the log line is about
// (an order id or a payment id: ours, not the provider's).
func result(ctx contextx.Contextx, topic, key string, err error) error {
	if err == nil {
		return nil
	}
	var de *kernel.Error
	if errors.As(err, &de) {
		ctx.Info("event rejected by domain", "topic", topic, "key", key, "code", de.Code())

		return fmt.Errorf("%w: %w", ErrDrop, err)
	}

	return err // infrastructure → retry, then poison
}

// OrderConfirmedHandler consumes ordering.v1.OrderConfirmed (T1, T14).
type OrderConfirmedHandler struct{ uc OrderConfirmedUC }

func NewOrderConfirmedHandler(uc OrderConfirmedUC) *OrderConfirmedHandler {
	return &OrderConfirmedHandler{uc: uc}
}

func (h *OrderConfirmedHandler) Handle(c context.Context, payload []byte) error {
	ctx := contextx.FromContext(c)
	var evt orderingv1.OrderConfirmed
	if err := decode(ctx, TopicOrderConfirmed, payload, &evt); err != nil {
		return err
	}
	in := dto.OrderConfirmedInput{OrderID: evt.GetOrderId(), CustomerID: evt.GetCustomerId()}
	// An event without a total (published before the field existed) stays nil here: the use case
	// refuses it, it is never read as a charge of zero (docs/payment.md §2).
	if t := evt.GetTotal(); t != nil {
		in.Total = &dto.MoneyOutput{Amount: t.GetAmountMinor(), Currency: t.GetCurrency()}
	}
	_, err := h.uc.Execute(ctx, in)

	return result(ctx, TopicOrderConfirmed, evt.GetOrderId(), err)
}

// OrderCancelledHandler consumes ordering.v1.OrderCancelled (T7, T10, T12, T13, T15).
type OrderCancelledHandler struct{ uc OrderCancelledUC }

func NewOrderCancelledHandler(uc OrderCancelledUC) *OrderCancelledHandler {
	return &OrderCancelledHandler{uc: uc}
}

func (h *OrderCancelledHandler) Handle(c context.Context, payload []byte) error {
	ctx := contextx.FromContext(c)
	var evt orderingv1.OrderCancelled
	if err := decode(ctx, TopicOrderCancelled, payload, &evt); err != nil {
		return err
	}
	_, err := h.uc.Execute(ctx, dto.OrderEndedInput{OrderID: evt.GetOrderId()})

	return result(ctx, TopicOrderCancelled, evt.GetOrderId(), err)
}

// OrderCompletedHandler consumes ordering.v1.OrderCompleted (T7', T12, T13, T15).
type OrderCompletedHandler struct{ uc OrderCompletedUC }

func NewOrderCompletedHandler(uc OrderCompletedUC) *OrderCompletedHandler {
	return &OrderCompletedHandler{uc: uc}
}

func (h *OrderCompletedHandler) Handle(c context.Context, payload []byte) error {
	ctx := contextx.FromContext(c)
	var evt orderingv1.OrderCompleted
	if err := decode(ctx, TopicOrderCompleted, payload, &evt); err != nil {
		return err
	}
	_, err := h.uc.Execute(ctx, dto.OrderEndedInput{OrderID: evt.GetOrderId()})

	return result(ctx, TopicOrderCompleted, evt.GetOrderId(), err)
}

// RefundHandler consumes payment.v1.PaymentRefundRequested (T11).
type RefundHandler struct{ uc RefundUC }

func NewRefundHandler(uc RefundUC) *RefundHandler { return &RefundHandler{uc: uc} }

func (h *RefundHandler) Handle(c context.Context, payload []byte) error {
	ctx := contextx.FromContext(c)
	var evt paymentv1.PaymentRefundRequested
	if err := decode(ctx, TopicPaymentRefundRequested, payload, &evt); err != nil {
		return err
	}
	_, err := h.uc.Execute(ctx, dto.RefundInput{PaymentID: evt.GetPaymentId()})

	return result(ctx, TopicPaymentRefundRequested, evt.GetPaymentId(), err)
}

// WebhookReceivedHandler consumes payment.v1.WebhookReceived: apply a stored provider notification
// (T3 / T4 / T8 / T9, the D and E columns).
type WebhookReceivedHandler struct{ uc WebhookUC }

func NewWebhookReceivedHandler(uc WebhookUC) *WebhookReceivedHandler {
	return &WebhookReceivedHandler{uc: uc}
}

func (h *WebhookReceivedHandler) Handle(c context.Context, payload []byte) error {
	ctx := contextx.FromContext(c)
	var evt paymentv1.WebhookReceived
	if err := decode(ctx, TopicWebhookReceived, payload, &evt); err != nil {
		return err
	}
	_, err := h.uc.Execute(ctx, dto.WebhookReceivedInput{Provider: evt.GetProvider(), EventID: evt.GetProviderEventId()})

	// The key names a stored row (ours); the provider's event id is a hash-like opaque string, but it is
	// the provider's, so the log line carries the provider name and a fixed code, never the id.
	return result(ctx, TopicWebhookReceived, evt.GetProvider(), err)
}
