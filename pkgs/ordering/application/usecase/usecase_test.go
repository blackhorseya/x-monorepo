package usecase_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/ownership"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/memory"
)

type fixedIDs struct{ id string }

func (i fixedIDs) NewID(context.Context) (string, error) { return i.id, nil }

type fixedClock struct{ t time.Time }

func (i fixedClock) Now() time.Time { return i.t }

// recorder is a port.EventPublisher that keeps what was published, in order.
type recorder struct{ events []order.Event }

func (r *recorder) Publish(_ context.Context, evs ...order.Event) error {
	r.events = append(r.events, evs...)
	return nil
}

func TestConfirmThenCancel(t *testing.T) {
	c := context.Background()
	repo := memory.NewOrderRepository()
	clock := fixedClock{t: time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)}
	rec := &recorder{}
	create := usecase.NewCreateOrder(repo, memory.NewTransactor(), rec, fixedIDs{id: "o-1"}, clock, ownership.New("admin"), memory.NewFlatPriceList(500, "USD"))
	confirm := usecase.NewConfirmOrder(repo, rec, clock)
	cancel := usecase.NewCancelOrder(repo, rec, clock, ownership.New("admin"))

	if _, err := create.Execute(c, dto.CreateOrderInput{
		CustomerID: "c-1",
		Items:      []dto.OrderLine{{SKU: "SKU-1", Quantity: 1}},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	confirmed, err := confirm.Execute(c, dto.ConfirmOrderInput{OrderID: "o-1"})
	if err != nil || confirmed.Status != "CONFIRMED" {
		t.Fatalf("confirm = %+v, %v", confirmed, err)
	}
	// StockReserved redelivered: the aggregate refuses, the consumer drops.
	if _, err = confirm.Execute(c, dto.ConfirmOrderInput{OrderID: "o-1"}); !errors.Is(err, order.ErrCannotConfirm) {
		t.Fatalf("second confirm err = %v, want ErrCannotConfirm", err)
	}
	if _, err = confirm.Execute(c, dto.ConfirmOrderInput{}); !errors.Is(err, order.ErrInvalidID) {
		t.Fatalf("empty id err = %v, want ErrInvalidID", err)
	}
	if _, err = confirm.Execute(c, dto.ConfirmOrderInput{OrderID: "missing"}); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("missing err = %v, want ErrNotFound", err)
	}

	// Confirmed is still cancellable; a StockReserved arriving after that is refused.
	cancelled, err := cancel.Execute(c, dto.CancelOrderInput{OrderID: "o-1"})
	if err != nil || cancelled.Status != "CANCELLED" {
		t.Fatalf("cancel = %+v, %v", cancelled, err)
	}
	if _, err = confirm.Execute(c, dto.ConfirmOrderInput{OrderID: "o-1"}); !errors.Is(err, order.ErrCannotConfirm) {
		t.Fatalf("confirm after cancel err = %v, want ErrCannotConfirm", err)
	}

	if len(rec.events) != 3 {
		t.Fatalf("published %d events, want 3 (Created, Confirmed, Cancelled)", len(rec.events))
	}
	if _, ok := rec.events[1].(order.Confirmed); !ok || rec.events[1].OrderID() != "o-1" {
		t.Fatalf("event[1] = %T %v", rec.events[1], rec.events[1])
	}
}

func TestCompleteOrder(t *testing.T) {
	c := context.Background()
	repo := memory.NewOrderRepository()
	clock := fixedClock{t: time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)}
	rec := &recorder{}
	create := usecase.NewCreateOrder(repo, memory.NewTransactor(), rec, fixedIDs{id: "o-1"}, clock, ownership.New("admin"), memory.NewFlatPriceList(500, "USD"))
	confirm := usecase.NewConfirmOrder(repo, rec, clock)
	complete := usecase.NewCompleteOrder(repo, rec, clock, ownership.New("admin"))
	cancel := usecase.NewCancelOrder(repo, rec, clock, ownership.New("admin"))

	if _, err := create.Execute(c, dto.CreateOrderInput{
		CustomerID: "c-1",
		Items:      []dto.OrderLine{{SKU: "SKU-1", Quantity: 1}},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Pending: inventory has not answered yet, nothing to commit.
	if _, err := complete.Execute(c, dto.CompleteOrderInput{OrderID: "o-1"}); !errors.Is(err, order.ErrCannotComplete) {
		t.Fatalf("complete while pending err = %v, want ErrCannotComplete", err)
	}
	if _, err := complete.Execute(c, dto.CompleteOrderInput{}); !errors.Is(err, order.ErrInvalidID) {
		t.Fatalf("empty id err = %v, want ErrInvalidID", err)
	}
	if _, err := complete.Execute(c, dto.CompleteOrderInput{OrderID: "missing"}); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("missing err = %v, want ErrNotFound", err)
	}

	if _, err := confirm.Execute(c, dto.ConfirmOrderInput{OrderID: "o-1"}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	done, err := complete.Execute(c, dto.CompleteOrderInput{OrderID: "o-1"})
	if err != nil || done.Status != "COMPLETED" {
		t.Fatalf("complete = %+v, %v", done, err)
	}
	// Terminal: a double click and a late cancel are both refused.
	if _, err = complete.Execute(c, dto.CompleteOrderInput{OrderID: "o-1"}); !errors.Is(err, order.ErrCannotComplete) {
		t.Fatalf("second complete err = %v, want ErrCannotComplete", err)
	}
	if _, err = cancel.Execute(c, dto.CancelOrderInput{OrderID: "o-1"}); !errors.Is(err, order.ErrCannotCancel) {
		t.Fatalf("cancel after complete err = %v, want ErrCannotCancel", err)
	}

	if len(rec.events) != 3 {
		t.Fatalf("published %d events, want 3 (Created, Confirmed, Completed)", len(rec.events))
	}
	if _, ok := rec.events[2].(order.Completed); !ok || rec.events[2].OrderID() != "o-1" {
		t.Fatalf("event[2] = %T %v", rec.events[2], rec.events[2])
	}
}

func TestCreateGetCancel(t *testing.T) {
	c := context.Background()
	repo := memory.NewOrderRepository()
	clock := fixedClock{t: time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)}

	rec := &recorder{}
	create := usecase.NewCreateOrder(repo, memory.NewTransactor(), rec, fixedIDs{id: "o-1"}, clock, ownership.New("admin"), memory.NewFlatPriceList(500, "USD"))
	get := usecase.NewGetOrder(repo, ownership.New("admin"))
	cancel := usecase.NewCancelOrder(repo, rec, clock, ownership.New("admin"))

	created, err := create.Execute(c, dto.CreateOrderInput{
		CustomerID: "c-1",
		Items: []dto.OrderLine{
			{SKU: "SKU-1", Quantity: 3},
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

	// Exactly one event per successful state change, published through the transactor.
	if len(rec.events) != 2 {
		t.Fatalf("published %d events, want 2 (Created, Cancelled)", len(rec.events))
	}
	if _, ok := rec.events[0].(order.Created); !ok || rec.events[0].OrderID() != "o-1" {
		t.Fatalf("event[0] = %T %v", rec.events[0], rec.events[0])
	}
	if _, ok := rec.events[1].(order.Cancelled); !ok {
		t.Fatalf("event[1] = %T", rec.events[1])
	}
}
