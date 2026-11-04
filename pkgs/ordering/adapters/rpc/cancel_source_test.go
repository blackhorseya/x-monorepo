package rpc_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/rpc"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// A client's CancelOrder is a cancellation by request: the adapter says so, because the use case
// (shared with the consumer) cannot tell the two callers apart on its own.
func TestCancelOrder_AdapterSaysItIsAnRPCRequest(t *testing.T) {
	var got dto.CancelOrderInput
	calls := 0
	cancel := usecasex.Func[dto.CancelOrderInput, *dto.OrderOutput](func(_ context.Context, in dto.CancelOrderInput) (*dto.OrderOutput, error) {
		got = in
		calls++

		return &dto.OrderOutput{ID: in.OrderID, Status: "CANCELLED"}, nil
	})
	svc := rpc.NewOrderService(nil, nil, cancel, nil, nil)

	if _, err := svc.CancelOrder(context.Background(), connect.NewRequest(&orderingv1.CancelOrderRequest{Id: "o-1"})); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || got.OrderID != "o-1" || got.Source != dto.CancelSourceRPC {
		t.Fatalf("use case called %d times with %+v, want once with OrderID o-1 and Source CancelSourceRPC", calls, got)
	}
}
