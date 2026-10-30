package consumer_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"

	inventoryv1 "github.com/blackhorseya/x-monorepo/pkgs/inventory/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/consumer"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/ownership"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// StockReserved from inventory confirms the order; a redelivery and a late one
// (order cancelled meanwhile) are domain rejections → drop, never retry.
func TestStockReservedHandler(t *testing.T) {
	c := context.Background()
	repo := memory.NewOrderRepository()
	create := usecase.NewCreateOrder(repo, noEvents{}, ids{}, clock{}, ownership.New("admin"), memory.NewFlatPriceList(500, "USD"))
	get := usecase.NewGetOrder(repo, ownership.New("admin"))
	cancel := usecase.NewCancelOrder(repo, noEvents{}, clock{}, ownership.New("admin"))
	h := consumer.NewStockReservedHandler(usecase.NewConfirmOrder(repo, noEvents{}, clock{}))

	if _, err := create.Execute(c, dto.CreateOrderInput{
		CustomerID: "c-1",
		Items:      []dto.OrderLine{{SKU: "S", Quantity: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	payload, _ := proto.Marshal(&inventoryv1.StockReserved{OrderId: "o-1", Lines: []*inventoryv1.StockReservedLine{{Sku: "S", Quantity: 1}}})

	if err := h.Handle(c, payload); err != nil {
		t.Fatalf("first handle: %v", err)
	}
	if got, err := get.Execute(c, dto.GetOrderInput{OrderID: "o-1"}); err != nil || got.Status != "CONFIRMED" {
		t.Fatalf("after StockReserved: %+v, %v", got, err)
	}
	if err := h.Handle(c, payload); !errors.Is(err, consumer.ErrDrop) {
		t.Fatalf("redelivery err = %v, want ErrDrop", err)
	}
	if _, err := cancel.Execute(c, dto.CancelOrderInput{OrderID: "o-1"}); err != nil {
		t.Fatalf("cancel confirmed order: %v", err)
	}
	if err := h.Handle(c, payload); !errors.Is(err, consumer.ErrDrop) {
		t.Fatalf("late StockReserved on a cancelled order err = %v, want ErrDrop", err)
	}
	if err := h.Handle(c, []byte{0xff, 0xff}); !errors.Is(err, consumer.ErrDrop) {
		t.Fatalf("garbage err = %v, want ErrDrop", err)
	}
	unknown, _ := proto.Marshal(&inventoryv1.StockReserved{OrderId: "nope"})
	if err := h.Handle(c, unknown); !errors.Is(err, consumer.ErrDrop) || !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("unknown order err = %v, want ErrDrop wrapping ErrNotFound", err)
	}
}

func TestStockReservedHandler_StaleIsRetried(t *testing.T) {
	stale := usecasex.Func[dto.ConfirmOrderInput, *dto.OrderOutput](func(context.Context, dto.ConfirmOrderInput) (*dto.OrderOutput, error) {
		return nil, fmt.Errorf("save order: %w", order.ErrStale)
	})
	payload, _ := proto.Marshal(&inventoryv1.StockReserved{OrderId: "o-1"})

	err := consumer.NewStockReservedHandler(stale).Handle(context.Background(), payload)
	if err == nil || errors.Is(err, consumer.ErrDrop) || !errors.Is(err, order.ErrStale) {
		t.Fatalf("err = %v, want ErrStale returned for retry (not ErrDrop)", err)
	}
}
