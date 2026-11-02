package consumer_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/adapters/consumer"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	paymentv1 "github.com/blackhorseya/x-monorepo/pkgs/payment/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	commonv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/common/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

func marshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	return b
}

// capture is a use case that records its input and answers with err.
func capture[I, O any](got *[]I, out O, err error) usecasex.UseCase[I, O] {
	return usecasex.Func[I, O](func(_ context.Context, in I) (O, error) {
		*got = append(*got, in)

		return out, err
	})
}

func TestOrderConfirmedHandler(t *testing.T) {
	c := context.Background()
	var got []dto.OrderConfirmedInput
	h := consumer.NewOrderConfirmedHandler(capture(&got, &dto.Ack{}, nil))

	evt := &orderingv1.OrderConfirmed{OrderId: "ord-1", CustomerId: "cust", Total: &commonv1.Money{AmountMinor: 1500, Currency: "TWD"}}
	if err := h.Handle(c, marshal(t, evt)); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].OrderID != "ord-1" || got[0].CustomerID != "cust" || got[0].Total == nil || got[0].Total.Amount != 1500 || got[0].Total.Currency != "TWD" {
		t.Fatalf("use case input = %+v", got)
	}

	// an event published before the total existed reaches the use case as "no total", not as zero
	got = nil
	if err := h.Handle(c, marshal(t, &orderingv1.OrderConfirmed{OrderId: "ord-2", CustomerId: "cust"})); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Total != nil {
		t.Fatalf("an event without a total reached the use case as %+v", got)
	}
}

func TestOrderEndedHandlers(t *testing.T) {
	c := context.Background()
	var cancelled, completed []dto.OrderEndedInput
	hc := consumer.NewOrderCancelledHandler(capture(&cancelled, &dto.OrderEndedOutput{}, nil))
	hd := consumer.NewOrderCompletedHandler(capture(&completed, &dto.OrderEndedOutput{}, nil))
	if err := hc.Handle(c, marshal(t, &orderingv1.OrderCancelled{OrderId: "ord-1"})); err != nil {
		t.Fatal(err)
	}
	if err := hd.Handle(c, marshal(t, &orderingv1.OrderCompleted{OrderId: "ord-2"})); err != nil {
		t.Fatal(err)
	}
	if len(cancelled) != 1 || cancelled[0].OrderID != "ord-1" || len(completed) != 1 || completed[0].OrderID != "ord-2" {
		t.Fatalf("cancelled %+v completed %+v", cancelled, completed)
	}
}

func TestRefundHandler(t *testing.T) {
	var got []dto.RefundInput
	h := consumer.NewRefundHandler(capture(&got, &dto.Ack{}, nil))
	if err := h.Handle(context.Background(), marshal(t, &paymentv1.PaymentRefundRequested{PaymentId: "pay-1"})); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].PaymentID != "pay-1" {
		t.Fatalf("input = %+v", got)
	}
}

// The error contract: undecodable and domain rejections are final (ErrDrop), everything else is
// retried (and finally parked): a lost lock, the database, the provider not answering.
func TestErrorContract(t *testing.T) {
	c := context.Background()
	domain := kernel.NewError(kernel.KindInvalidArgument, "payment.missing_total", "no total")
	infra := errors.New("connection reset")

	type handler interface {
		Handle(context.Context, []byte) error
	}
	good := map[string][]byte{
		"confirmed": marshal(t, &orderingv1.OrderConfirmed{OrderId: "o"}),
		"cancelled": marshal(t, &orderingv1.OrderCancelled{OrderId: "o"}),
		"completed": marshal(t, &orderingv1.OrderCompleted{OrderId: "o"}),
		"refund":    marshal(t, &paymentv1.PaymentRefundRequested{PaymentId: "p"}),
	}
	build := func(err error) map[string]handler {
		var a []dto.OrderConfirmedInput
		var b, d []dto.OrderEndedInput
		var r []dto.RefundInput

		return map[string]handler{
			"confirmed": consumer.NewOrderConfirmedHandler(capture(&a, &dto.Ack{}, err)),
			"cancelled": consumer.NewOrderCancelledHandler(capture(&b, &dto.OrderEndedOutput{}, err)),
			"completed": consumer.NewOrderCompletedHandler(capture(&d, &dto.OrderEndedOutput{}, err)),
			"refund":    consumer.NewRefundHandler(capture(&r, &dto.Ack{}, err)),
		}
	}

	for name, h := range build(domain) {
		if err := h.Handle(c, good[name]); !errors.Is(err, eventbus.ErrDrop) || !errors.Is(err, domain) {
			t.Fatalf("%s with a domain rejection: %v, want a drop that keeps the cause", name, err)
		}
	}
	for name, h := range build(infra) {
		if err := h.Handle(c, good[name]); !errors.Is(err, infra) || errors.Is(err, eventbus.ErrDrop) {
			t.Fatalf("%s with an infrastructure error: %v, want it retried", name, err)
		}
	}
	for name, h := range build(nil) {
		if err := h.Handle(c, []byte{0xff, 0xff, 0xff}); !errors.Is(err, eventbus.ErrDrop) {
			t.Fatalf("%s with garbage: %v, want a drop", name, err)
		}
	}
	// the topics are the proto names
	if consumer.TopicOrderConfirmed != "ordering.v1.OrderConfirmed" || consumer.TopicPaymentRefundRequested != "payment.v1.PaymentRefundRequested" {
		t.Fatalf("topics: %q %q", consumer.TopicOrderConfirmed, consumer.TopicPaymentRefundRequested)
	}
	_ = payment.ErrNotFound
}
