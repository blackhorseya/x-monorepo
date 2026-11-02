package events_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"google.golang.org/protobuf/proto"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/adapters/events"
	paymentv1 "github.com/blackhorseya/x-monorepo/pkgs/payment/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
)

type sent struct {
	topic string
	msg   *message.Message
}

type fakePub struct {
	mu   sync.Mutex
	sent []sent
	err  error
}

func (f *fakePub) Publish(topic string, msgs ...*message.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range msgs {
		f.sent = append(f.sent, sent{topic, m})
	}

	return f.err
}
func (f *fakePub) Close() error { return nil }

var at = time.Date(2026, 11, 2, 1, 2, 3, 0, time.UTC)

func aPayment(t *testing.T) *payment.Payment {
	t.Helper()
	total, err := kernel.NewMoney(1500, "TWD")
	if err != nil {
		t.Fatal(err)
	}
	p, err := payment.New(payment.NewParams{ID: "pay-1", OrderID: "ord-1", CustomerID: "cust", Total: total, ExpiresAt: at.Add(time.Hour)}, at)
	if err != nil {
		t.Fatal(err)
	}

	return p
}

// Each payment.Event becomes its payment.v1 message on its topic, keyed by the order, stamped with the
// domain time; the wire carries the payment's own facts.
func TestPublish_MapsEveryEvent(t *testing.T) {
	pub := &fakePub{}
	h := events.NewPublisher(pub)
	p := aPayment(t)
	all := []payment.Event{payment.EventCaptured, payment.EventExpired, payment.EventRefundRequested, payment.EventRefunded}
	if err := h.Publish(context.Background(), p, all, at); err != nil {
		t.Fatal(err)
	}
	if len(pub.sent) != 4 {
		t.Fatalf("%d messages, want 4", len(pub.sent))
	}
	wantTopics := []string{"payment.v1.PaymentCaptured", "payment.v1.PaymentExpired", "payment.v1.PaymentRefundRequested", "payment.v1.PaymentRefunded"}
	for i, s := range pub.sent {
		if s.topic != wantTopics[i] {
			t.Fatalf("message %d on %q, want %q", i, s.topic, wantTopics[i])
		}
		if s.msg.Metadata.Get(eventbus.MetaKey) != "ord-1" || s.msg.Metadata.Get(eventbus.MetaOccurredAt) == "" || s.msg.UUID == "" {
			t.Fatalf("message %d metadata %v uuid %q", i, s.msg.Metadata, s.msg.UUID)
		}
	}

	var cap paymentv1.PaymentCaptured
	if err := proto.Unmarshal(pub.sent[0].msg.Payload, &cap); err != nil {
		t.Fatal(err)
	}
	if cap.GetPaymentId() != "pay-1" || cap.GetOrderId() != "ord-1" || cap.GetCustomerId() != "cust" ||
		cap.GetAmount().GetAmountMinor() != 1500 || cap.GetAmount().GetCurrency() != "TWD" || !cap.GetCapturedAt().AsTime().Equal(at) {
		t.Fatalf("PaymentCaptured = %v", &cap)
	}
	var exp paymentv1.PaymentExpired
	_ = proto.Unmarshal(pub.sent[1].msg.Payload, &exp)
	if exp.GetPaymentId() != "pay-1" || exp.GetOrderId() != "ord-1" || exp.GetCustomerId() != "cust" || !exp.GetExpiredAt().AsTime().Equal(at) {
		t.Fatalf("PaymentExpired = %v", &exp)
	}
	var req paymentv1.PaymentRefundRequested
	_ = proto.Unmarshal(pub.sent[2].msg.Payload, &req)
	if req.GetPaymentId() != "pay-1" {
		t.Fatalf("PaymentRefundRequested = %v", &req)
	}
	var ref paymentv1.PaymentRefunded
	_ = proto.Unmarshal(pub.sent[3].msg.Payload, &ref)
	if ref.GetPaymentId() != "pay-1" || ref.GetAmount().GetAmountMinor() != 1500 || !ref.GetRefundedAt().AsTime().Equal(at) {
		t.Fatalf("PaymentRefunded = %v", &ref)
	}
}

func TestPublish_NothingToSayPublishesNothing(t *testing.T) {
	pub := &fakePub{}
	if err := events.NewPublisher(pub).Publish(context.Background(), aPayment(t), nil, at); err != nil || len(pub.sent) != 0 {
		t.Fatalf("sent %d, err %v", len(pub.sent), err)
	}
}

func TestPublish_Errors(t *testing.T) {
	h := events.NewPublisher(&fakePub{})
	if err := h.Publish(context.Background(), aPayment(t), []payment.Event{"PaymentNonsense"}, at); err == nil {
		t.Fatal("an event with no wire mapping must be an error, not silently dropped")
	}
	failing := events.NewPublisher(&fakePub{err: context.DeadlineExceeded})
	if err := failing.Publish(context.Background(), aPayment(t), []payment.Event{payment.EventCaptured}, at); err == nil {
		t.Fatal("a publisher failure must reach the caller (the transaction rolls back)")
	}
	if err := failing.PublishWebhookReceived(context.Background(), "mock", "evt-1", at); err == nil {
		t.Fatal("PublishWebhookReceived swallowed a publisher failure")
	}
}

func TestPublishWebhookReceived(t *testing.T) {
	pub := &fakePub{}
	if err := events.NewPublisher(pub).PublishWebhookReceived(context.Background(), "mock", "evt-9", at); err != nil {
		t.Fatal(err)
	}
	if len(pub.sent) != 1 || pub.sent[0].topic != "payment.v1.WebhookReceived" {
		t.Fatalf("sent %+v", pub.sent)
	}
	var w paymentv1.WebhookReceived
	if err := proto.Unmarshal(pub.sent[0].msg.Payload, &w); err != nil {
		t.Fatal(err)
	}
	if w.GetProvider() != "mock" || w.GetProviderEventId() != "evt-9" {
		t.Fatalf("WebhookReceived = %v", &w)
	}
}
