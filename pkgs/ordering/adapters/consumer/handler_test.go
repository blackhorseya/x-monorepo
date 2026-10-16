package consumer_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/consumer"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/usecase"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/memory"
)

type clock struct{}

func (clock) Now() time.Time { return time.Now() }

type ids struct{}

func (ids) NewID(context.Context) (string, error) { return "o-1", nil }

// The consumer and the rpc adapter share one CancelOrder use case; here we
// drive it from an event and assert the same domain outcome and error contract.
func TestOrderCancellationHandler(t *testing.T) {
	c := context.Background()
	repo := memory.NewOrderRepository()
	create := usecase.NewCreateOrder(repo, ids{}, clock{})
	h := consumer.NewOrderCancellationHandler(usecase.NewCancelOrder(repo, clock{}))

	if _, err := create.Execute(c, dto.CreateOrderInput{
		CustomerID: "c-1",
		Items:      []dto.LineItem{{SKU: "S", Quantity: 1, UnitPrice: dto.Money{AmountMinor: 1, Currency: "USD"}}},
	}); err != nil {
		t.Fatal(err)
	}

	payload, _ := proto.Marshal(&orderingv1.OrderCancellationRequested{OrderId: "o-1", Reason: "payment timeout"})

	if err := h.Handle(c, payload); err != nil {
		t.Fatalf("first handle: %v", err)
	}
	// Second delivery (at-least-once): domain says cannot cancel → drop, not retry.
	if err := h.Handle(c, payload); !errors.Is(err, consumer.ErrDrop) {
		t.Fatalf("redelivery err = %v, want ErrDrop", err)
	}
	// Garbage → drop.
	if err := h.Handle(c, []byte{0xff, 0xff}); !errors.Is(err, consumer.ErrDrop) {
		t.Fatalf("garbage err = %v, want ErrDrop", err)
	}
}
