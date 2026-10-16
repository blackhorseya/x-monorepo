package usecase_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/memory"
)

type fixedIDs struct{ id string }

func (i fixedIDs) NewID(context.Context) (string, error) { return i.id, nil }

type fixedClock struct{ t time.Time }

func (i fixedClock) Now() time.Time { return i.t }

func TestCreateGetCancel(t *testing.T) {
	c := context.Background()
	repo := memory.NewOrderRepository()
	clock := fixedClock{t: time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)}

	create := usecase.NewCreateOrder(repo, fixedIDs{id: "o-1"}, clock)
	get := usecase.NewGetOrder(repo)
	cancel := usecase.NewCancelOrder(repo, clock)

	created, err := create.Execute(c, dto.CreateOrderInput{
		CustomerID: "c-1",
		Items: []dto.LineItem{
			{SKU: "SKU-1", Quantity: 3, UnitPrice: dto.Money{AmountMinor: 500, Currency: "USD"}},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Total.AmountMinor != 1500 {
		t.Fatalf("total = %d, want 1500", created.Total.AmountMinor)
	}

	got, err := get.Execute(c, dto.GetOrderInput{OrderID: "o-1"})
	if err != nil || got.Status != "PENDING" {
		t.Fatalf("get = %+v, %v", got, err)
	}

	cancelled, err := cancel.Execute(c, dto.CancelOrderInput{OrderID: "o-1"})
	if err != nil || cancelled.Status != "CANCELLED" {
		t.Fatalf("cancel = %+v, %v", cancelled, err)
	}

	if _, err = cancel.Execute(c, dto.CancelOrderInput{OrderID: "o-1"}); !errors.Is(err, order.ErrCannotCancel) {
		t.Fatalf("second cancel err = %v, want ErrCannotCancel", err)
	}

	if _, err = get.Execute(c, dto.GetOrderInput{OrderID: "missing"}); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("get missing err = %v, want ErrNotFound", err)
	}
}
