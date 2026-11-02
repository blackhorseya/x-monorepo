// Package events is the outbound adapter for payment's events: it maps the events a transition
// raised (payment.Event) on a payment to the payment.v1 wire contract and hands them to a
// Watermill publisher. Like adapters/rpc it is one of the few packages that import contract/.
package events

import (
	"context"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	paymentv1 "github.com/blackhorseya/x-monorepo/pkgs/payment/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
)

// Topics published by payment: the proto full names.
var (
	TopicPaymentCaptured        = string((&paymentv1.PaymentCaptured{}).ProtoReflect().Descriptor().FullName())
	TopicPaymentExpired         = string((&paymentv1.PaymentExpired{}).ProtoReflect().Descriptor().FullName())
	TopicPaymentRefunded        = string((&paymentv1.PaymentRefunded{}).ProtoReflect().Descriptor().FullName())
	TopicPaymentRefundRequested = string((&paymentv1.PaymentRefundRequested{}).ProtoReflect().Descriptor().FullName())
	TopicWebhookReceived        = string((&paymentv1.WebhookReceived{}).ProtoReflect().Descriptor().FullName())
)

var _ port.EventPublisher = (*Publisher)(nil)

// Publisher implements port.EventPublisher on a message.Publisher. With the Postgres publisher the
// rows join the caller's transaction (the context must come from Transactor.Do or the Transactional
// decorator).
type Publisher struct {
	pub message.Publisher
}

func NewPublisher(pub message.Publisher) *Publisher { return &Publisher{pub: pub} }

func (h *Publisher) Publish(context.Context, *payment.Payment, []payment.Event, time.Time) error {
	return nil // SPEC STAGE
}

func (h *Publisher) PublishWebhookReceived(context.Context, string, string, time.Time) error {
	return nil // SPEC STAGE
}
