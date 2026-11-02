package consumer_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/consumer"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/ownership"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/memory"
	paymentv1 "github.com/blackhorseya/x-monorepo/pkgs/payment/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// payment's wire topics are the proto full names the outbox tables are named after
// (payment.events_v1_paymentcaptured / ..._paymentexpired).
func TestPaymentTopics(t *testing.T) {
	if consumer.TopicPaymentCaptured != "payment.v1.PaymentCaptured" || consumer.TopicPaymentExpired != "payment.v1.PaymentExpired" {
		t.Errorf("topics = %q, %q", consumer.TopicPaymentCaptured, consumer.TopicPaymentExpired)
	}
}

// payOrder is an order confirmed under the given setting, with real use cases behind the handlers.
func payOrder(t *testing.T, requirePayment bool) (*consumer.PaymentCapturedHandler, *consumer.PaymentExpiredHandler, *usecase.GetOrder) {
	t.Helper()
	c := context.Background()
	repo := memory.NewOrderRepository()
	own := ownership.New("admin")
	if _, err := usecase.NewCreateOrder(repo, memory.NewTransactor(), noEvents{}, ids{}, clock{}, own, memory.NewFlatPriceList(500, "USD")).
		Execute(c, dto.CreateOrderInput{CustomerID: "c-1", Items: []dto.OrderLine{{SKU: "S", Quantity: 1}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := usecase.NewConfirmOrder(repo, noEvents{}, clock{}, usecase.WithRequirePayment(requirePayment)).Execute(c, dto.ConfirmOrderInput{OrderID: "o-1"}); err != nil {
		t.Fatal(err)
	}

	return consumer.NewPaymentCapturedHandler(usecase.NewApplyPaymentCaptured(repo, noEvents{}, clock{})),
		consumer.NewPaymentExpiredHandler(usecase.NewApplyPaymentExpired(repo, noEvents{}, clock{})),
		usecase.NewGetOrder(repo, own)
}

// The error contract of the event bus: ack on success, ErrDrop (ack, no retry) on a domain rejection
// or an undecodable payload, anything else retries. PaymentCaptured for a CANCELLED order is the
// rejection that matters: dropped here, refunded by payment (T10).
func TestPaymentHandlers_ErrorContract(t *testing.T) {
	c := context.Background()
	cap, _ := proto.Marshal(&paymentv1.PaymentCaptured{PaymentId: "p-1", OrderId: "o-1", CustomerId: "c-1"})
	exp, _ := proto.Marshal(&paymentv1.PaymentExpired{PaymentId: "p-1", OrderId: "o-1", CustomerId: "c-1"})

	captured, expired, get := payOrder(t, true)
	if err := captured.Handle(c, cap); err != nil {
		t.Fatalf("captured: %v", err)
	}
	if got, _ := get.Execute(c, dto.GetOrderInput{OrderID: "o-1"}); got == nil || got.Status != "COMPLETED" {
		t.Fatalf("after PaymentCaptured: %+v", got)
	}
	if err := captured.Handle(c, cap); err != nil {
		t.Errorf("redelivered captured (a no-op) must ack: %v", err)
	}
	if err := expired.Handle(c, exp); !errors.Is(err, consumer.ErrDrop) || !errors.Is(err, order.ErrCannotCancel) {
		t.Errorf("expired after captured: %v, want ErrDrop wrapping order.cannot_cancel", err)
	}

	captured, expired, get = payOrder(t, true)
	if err := expired.Handle(c, exp); err != nil {
		t.Fatalf("expired: %v", err)
	}
	if got, _ := get.Execute(c, dto.GetOrderInput{OrderID: "o-1"}); got == nil || got.Status != "CANCELLED" {
		t.Fatalf("after PaymentExpired: %+v", got)
	}
	if err := captured.Handle(c, cap); !errors.Is(err, consumer.ErrDrop) || !errors.Is(err, order.ErrCannotPay) {
		t.Errorf("captured after expired: %v, want ErrDrop wrapping order.cannot_pay", err)
	}

	for name, h := range map[string]func(context.Context, []byte) error{"captured": captured.Handle, "expired": expired.Handle} {
		if err := h(c, []byte{0xff, 0xff, 0xff}); !errors.Is(err, consumer.ErrDrop) {
			t.Errorf("%s: undecodable payload err = %v, want ErrDrop", name, err)
		}
	}
	nothing, _ := proto.Marshal(&paymentv1.PaymentCaptured{})
	if err := captured.Handle(c, nothing); !errors.Is(err, consumer.ErrDrop) || !errors.Is(err, order.ErrInvalidID) {
		t.Errorf("no order id: %v, want ErrDrop wrapping order.invalid_id", err)
	}
}

var errDB = errors.New("connection reset")

type failing struct{}

func (failing) Execute(context.Context, dto.PaymentCapturedInput) (*dto.PaymentEventOutput, error) {
	return nil, errDB
}

type failingExpired struct{}

func (failingExpired) Execute(context.Context, dto.PaymentExpiredInput) (*dto.PaymentEventOutput, error) {
	return nil, errDB
}

// An infrastructure error — and the optimistic-lock conflict that outlasted the use case's retries —
// is retried by the router, never dropped: dropping would lose a payment that is real.
func TestPaymentHandlers_InfrastructureErrorsRetry(t *testing.T) {
	c := context.Background()
	cap, _ := proto.Marshal(&paymentv1.PaymentCaptured{OrderId: "o-1"})
	exp, _ := proto.Marshal(&paymentv1.PaymentExpired{OrderId: "o-1"})
	var _ usecasex.UseCase[dto.PaymentCapturedInput, *dto.PaymentEventOutput] = failing{}

	for name, err := range map[string]error{
		"captured": consumer.NewPaymentCapturedHandler(failing{}).Handle(c, cap),
		"expired":  consumer.NewPaymentExpiredHandler(failingExpired{}).Handle(c, exp),
	} {
		if !errors.Is(err, errDB) || errors.Is(err, consumer.ErrDrop) {
			t.Errorf("%s: err = %v, want the store's error, not ErrDrop", name, err)
		}
	}
}
