// Package events is the outbound adapter for payment's events: it maps the events a transition
// raised (payment.Event) on a payment to the payment.v1 wire contract and hands them to a
// Watermill publisher. Like adapters/rpc it is one of the few packages that import contract/.
package events

import (
	"context"
	"fmt"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	paymentv1 "github.com/blackhorseya/x-monorepo/pkgs/payment/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	commonv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/common/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/idgen"
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

func (h *Publisher) Publish(c context.Context, p *payment.Payment, events []payment.Event, at time.Time) error {
	for _, e := range events {
		topic, pb, err := toProto(p, e, at)
		if err != nil {
			return err
		}
		if err = h.send(c, topic, pb, p.OrderID(), at); err != nil {
			return fmt.Errorf("publish %s for order %s: %w", topic, p.OrderID(), err)
		}
	}

	return nil
}

func (h *Publisher) PublishWebhookReceived(c context.Context, provider, providerEventID string, at time.Time) error {
	err := h.send(c, TopicWebhookReceived, &paymentv1.WebhookReceived{Provider: provider, ProviderEventId: providerEventID}, provider+"/"+providerEventID, at)
	if err != nil {
		return fmt.Errorf("publish %s: %w", TopicWebhookReceived, err)
	}

	return nil
}

func (h *Publisher) send(c context.Context, topic string, pb proto.Message, key string, at time.Time) error {
	payload, err := proto.Marshal(pb)
	if err != nil {
		return fmt.Errorf("encode %s: %w", topic, err)
	}
	id, err := idgen.New()
	if err != nil {
		return fmt.Errorf("message id for %s: %w", topic, err)
	}
	msg := message.NewMessageWithContext(c, id, payload)
	msg.Metadata.Set(eventbus.MetaKey, key)
	msg.Metadata.Set(eventbus.MetaOccurredAt, at.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"))

	return h.pub.Publish(topic, msg)
}

func toProto(p *payment.Payment, e payment.Event, at time.Time) (string, proto.Message, error) {
	switch e {
	case payment.EventCaptured:
		return TopicPaymentCaptured, &paymentv1.PaymentCaptured{
			PaymentId: p.ID().String(), OrderId: p.OrderID(), CustomerId: p.CustomerID(),
			Amount:     &commonv1.Money{AmountMinor: p.Amount().Amount(), Currency: p.Amount().Currency()},
			CapturedAt: timestamppb.New(at),
		}, nil
	case payment.EventExpired:
		return TopicPaymentExpired, &paymentv1.PaymentExpired{
			PaymentId: p.ID().String(), OrderId: p.OrderID(), CustomerId: p.CustomerID(), ExpiredAt: timestamppb.New(at),
		}, nil
	case payment.EventRefunded:
		return TopicPaymentRefunded, &paymentv1.PaymentRefunded{
			PaymentId: p.ID().String(), OrderId: p.OrderID(),
			Amount:     &commonv1.Money{AmountMinor: p.Amount().Amount(), Currency: p.Amount().Currency()},
			RefundedAt: timestamppb.New(at),
		}, nil
	case payment.EventRefundRequested:
		return TopicPaymentRefundRequested, &paymentv1.PaymentRefundRequested{PaymentId: p.ID().String()}, nil
	default:
		return "", nil, fmt.Errorf("no wire mapping for payment event %q", string(e))
	}
}
