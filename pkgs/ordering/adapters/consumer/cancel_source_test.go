package consumer_test

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/consumer"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// An OrderCancellationRequested is another BC asking: the handler says so, and the free-text reason
// still travels (the use case ignores it; it is never stored or logged).
func TestOrderCancellationHandler_SaysItIsACancellationRequest(t *testing.T) {
	var got dto.CancelOrderInput
	calls := 0
	cancel := usecasex.Func[dto.CancelOrderInput, *dto.OrderOutput](func(_ context.Context, in dto.CancelOrderInput) (*dto.OrderOutput, error) {
		got = in
		calls++

		return &dto.OrderOutput{ID: in.OrderID, Status: "CANCELLED"}, nil
	})
	payload, _ := proto.Marshal(&orderingv1.OrderCancellationRequested{OrderId: "o-1", Reason: "reservation r-1 expired after 24h"})

	if err := consumer.NewOrderCancellationHandler(cancel).Handle(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || got.OrderID != "o-1" || got.Source != dto.CancelSourceCancellationRequest {
		t.Fatalf("use case called %d times with %+v, want once with OrderID o-1 and Source CancelSourceCancellationRequest", calls, got)
	}
}
