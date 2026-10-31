package rpc_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/rpc"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/port"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
)

type createOrderFails struct{ err error }

func (f createOrderFails) Execute(context.Context, dto.CreateOrderInput) (*dto.OrderOutput, error) {
	return nil, f.err
}

// The catalog not answering fails CreateOrder closed: UNAVAILABLE, x-error-code catalog.unavailable,
// a fixed message (the cause is the use case's log, not the caller's), and the same request can be
// retried. Other failures keep their usual mapping.
func TestCreateOrder_ACatalogThatDoesNotAnswerIsUnavailable(t *testing.T) {
	cause := connect.NewError(connect.CodeUnavailable, errors.New("dial tcp 10.0.0.7:80: connection refused"))
	svc := rpc.NewOrderService(createOrderFails{err: fmt.Errorf("quote prices of 1 skus: %w: %w", port.ErrCatalogUnavailable, cause)}, nil, nil, nil, nil)

	_, err := svc.CreateOrder(context.Background(), connect.NewRequest(&orderingv1.CreateOrderRequest{CustomerId: "c"}))
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeUnavailable || ce.Meta().Get("x-error-code") != "catalog.unavailable" {
		t.Fatalf("err = %v, want UNAVAILABLE with x-error-code catalog.unavailable", err)
	}
	if ce.Message() != "the catalog could not be reached, retry the request" {
		t.Fatalf("message = %q, want the fixed one (no cause)", ce.Message())
	}

	_, err = rpc.NewOrderService(createOrderFails{err: errors.New("boom")}, nil, nil, nil, nil).
		CreateOrder(context.Background(), connect.NewRequest(&orderingv1.CreateOrderRequest{CustomerId: "c"}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("an unrelated failure is %s, want INTERNAL", connect.CodeOf(err))
	}
}
