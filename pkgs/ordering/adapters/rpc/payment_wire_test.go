package rpc_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/rpc"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
)

type getOrderReturns struct{ out *dto.OrderOutput }

func (f getOrderReturns) Execute(context.Context, dto.GetOrderInput) (*dto.OrderOutput, error) {
	return f.out, nil
}

type completeOrderFails struct{ err error }

func (f completeOrderFails) Execute(context.Context, dto.CompleteOrderInput) (*dto.OrderOutput, error) {
	return nil, f.err
}

type listReturns struct{ gotStatus *string }

func (f listReturns) Execute(_ context.Context, in dto.ListOrdersInput) (*dto.ListOrdersOutput, error) {
	*f.gotStatus = in.Status

	return &dto.ListOrdersOutput{}, nil
}

// The wire carries PAID and payment_required (docs/payment.md §2), and the PAID list filter
// reaches the use case by name.
func TestWire_PaidAndPaymentRequired(t *testing.T) {
	c := context.Background()
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	for _, required := range []bool{true, false} {
		svc := rpc.NewOrderService(nil, getOrderReturns{&dto.OrderOutput{ID: "o-1", Status: "PAID", PaymentRequired: required, CreatedAt: at, UpdatedAt: at}}, nil, nil, nil)
		res, err := svc.GetOrder(c, connect.NewRequest(&orderingv1.GetOrderRequest{Id: "o-1"}))
		if err != nil {
			t.Fatal(err)
		}
		if got := res.Msg.GetOrder(); got.GetStatus() != orderingv1.OrderStatus_ORDER_STATUS_PAID || got.GetPaymentRequired() != required {
			t.Fatalf("GetOrder = %v, want PAID with payment_required %v", got, required)
		}
	}

	var status string
	svc := rpc.NewOrderService(nil, nil, nil, nil, listReturns{&status})
	if _, err := svc.ListOrders(c, connect.NewRequest(&orderingv1.ListOrdersRequest{Status: orderingv1.OrderStatus_ORDER_STATUS_PAID})); err != nil {
		t.Fatal(err)
	}
	if status != "PAID" {
		t.Fatalf("ListOrders filter reached the use case as %q, want PAID", status)
	}
}

// CompleteOrder on an order that requires payment is FAILED_PRECONDITION with the x-error-code
// order.payment_required (README error table).
func TestWire_CompleteOrder_PaymentRequired(t *testing.T) {
	svc := rpc.NewOrderService(nil, nil, nil, completeOrderFails{err: order.ErrPaymentRequired}, nil)
	_, err := svc.CompleteOrder(context.Background(), connect.NewRequest(&orderingv1.CompleteOrderRequest{Id: "o-1"}))
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeFailedPrecondition || ce.Meta().Get("x-error-code") != "order.payment_required" {
		t.Fatalf("err = %v, want FAILED_PRECONDITION / order.payment_required", err)
	}
}
