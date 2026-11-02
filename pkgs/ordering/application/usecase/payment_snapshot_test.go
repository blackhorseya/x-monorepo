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

// docs/payment.md §8, ordering table, CompleteOrder / CancelOrder columns, for every cell slice 1
// can reach through use cases: snapshot × status. (true/PAID and true/COMPLETED need a payment
// event; they are covered by the domain table in domain/order/payment_test.go.)
//
// The snapshot is made by ConfirmOrder with the setting given to it. The "default" rows build
// ConfirmOrder with no option at all: they test the default value of the setting.
func TestPaymentSnapshot_CompleteAndCancel_ByCell(t *testing.T) {
	type confirmSetting struct {
		name string
		opts []usecase.ConfirmOption
		want bool
	}
	settings := []confirmSetting{
		{"default (no option)", nil, false},
		{"require_payment=false", []usecase.ConfirmOption{usecase.WithRequirePayment(false)}, false},
		{"require_payment=true", []usecase.ConfirmOption{usecase.WithRequirePayment(true)}, true},
	}
	const (
		pending   = "PENDING"
		confirmed = "CONFIRMED"
		cancelled = "CANCELLED"
		completed = "COMPLETED"
	)
	// reach drives the order to a status; the cells below are (setting, status).
	type cell struct {
		setting  int
		reach    string
		complete error // nil = COMPLETED
		cancel   error // nil = CANCELLED
	}
	cells := []cell{
		{0, pending, order.ErrCannotComplete, nil},
		{0, confirmed, nil, nil},
		{0, cancelled, order.ErrCannotComplete, order.ErrCannotCancel},
		{0, completed, order.ErrCannotComplete, order.ErrCannotCancel},
		{1, confirmed, nil, nil},
		{2, pending, order.ErrCannotComplete, nil},
		{2, confirmed, order.ErrPaymentRequired, nil},
		{2, cancelled, order.ErrCannotComplete, order.ErrCannotCancel},
	}
	for _, tc := range cells {
		s := settings[tc.setting]
		t.Run(s.name+"/"+tc.reach, func(t *testing.T) {
			// Each action runs on its own fresh order so one cannot disturb the other.
			for _, action := range []string{"complete", "cancel"} {
				c := context.Background()
				repo := memory.NewOrderRepository()
				clock := fixedClock{t: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
				rec := &recorder{}
				own := ownership.New("admin")
				create := usecase.NewCreateOrder(repo, memory.NewTransactor(), rec, fixedIDs{id: "o-1"}, clock, own, memory.NewFlatPriceList(500, "USD"))
				confirm := usecase.NewConfirmOrder(repo, rec, clock, s.opts...)
				complete := usecase.NewCompleteOrder(repo, rec, clock, own)
				cancel := usecase.NewCancelOrder(repo, rec, clock, own)

				if _, err := create.Execute(c, dto.CreateOrderInput{CustomerID: "c-1", Items: []dto.OrderLine{{SKU: "S", Quantity: 1}}}); err != nil {
					t.Fatal(err)
				}
				switch tc.reach {
				case confirmed, cancelled, completed:
					if out, err := confirm.Execute(c, dto.ConfirmOrderInput{OrderID: "o-1"}); err != nil || out.PaymentRequired != s.want {
						t.Fatalf("confirm = %+v, %v; want PaymentRequired %v", out, err, s.want)
					}
				}
				switch tc.reach {
				case cancelled:
					if _, err := cancel.Execute(c, dto.CancelOrderInput{OrderID: "o-1"}); err != nil {
						t.Fatal(err)
					}
				case completed:
					if _, err := complete.Execute(c, dto.CompleteOrderInput{OrderID: "o-1"}); err != nil {
						t.Fatal(err)
					}
				}

				var (
					out     *dto.OrderOutput
					err     error
					wantErr error
					wantSt  string
				)
				if action == "complete" {
					out, err = complete.Execute(c, dto.CompleteOrderInput{OrderID: "o-1"})
					wantErr, wantSt = tc.complete, completed
				} else {
					out, err = cancel.Execute(c, dto.CancelOrderInput{OrderID: "o-1"})
					wantErr, wantSt = tc.cancel, cancelled
				}
				if !errors.Is(err, wantErr) {
					t.Fatalf("%s err = %v, want %v", action, err, wantErr)
				}
				got, gerr := usecase.NewGetOrder(repo, own).Execute(c, dto.GetOrderInput{OrderID: "o-1"})
				if gerr != nil {
					t.Fatal(gerr)
				}
				switch {
				case wantErr == nil && (out.Status != wantSt || got.Status != wantSt):
					t.Fatalf("%s: status = %s / stored %s, want %s", action, out.Status, got.Status, wantSt)
				case wantErr != nil && got.Status != tc.reach:
					t.Fatalf("a refused %s changed the status to %s (was %s)", action, got.Status, tc.reach)
				}
				if tc.reach != pending && got.PaymentRequired != s.want {
					t.Fatalf("stored PaymentRequired = %v, want %v", got.PaymentRequired, s.want)
				}
			}
		})
	}
}

// The snapshot belongs to the order: a second ConfirmOrder built with the opposite setting (the
// setting was flipped and the worker restarted) neither changes an order already CONFIRMED nor
// lets CompleteOrder past it.
func TestPaymentSnapshot_SettingFlippedLater(t *testing.T) {
	c := context.Background()
	repo := memory.NewOrderRepository()
	clock := fixedClock{t: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
	rec := &recorder{}
	own := ownership.New("admin")
	ids := &seqIDs{}
	create := usecase.NewCreateOrder(repo, memory.NewTransactor(), rec, ids, clock, own, memory.NewFlatPriceList(500, "USD"))
	for range 2 {
		if _, err := create.Execute(c, dto.CreateOrderInput{CustomerID: "c-1", Items: []dto.OrderLine{{SKU: "S", Quantity: 1}}}); err != nil {
			t.Fatal(err)
		}
	}
	complete := usecase.NewCompleteOrder(repo, rec, clock, own)

	on := usecase.NewConfirmOrder(repo, rec, clock, usecase.WithRequirePayment(true))
	if out, err := on.Execute(c, dto.ConfirmOrderInput{OrderID: "o-a"}); err != nil || !out.PaymentRequired {
		t.Fatalf("confirm under true = %+v, %v", out, err)
	}
	// Setting flipped back to the default; the same order is confirmed again (redelivery) and still refused.
	off := usecase.NewConfirmOrder(repo, rec, clock)
	if _, err := off.Execute(c, dto.ConfirmOrderInput{OrderID: "o-a"}); !errors.Is(err, order.ErrCannotConfirm) {
		t.Fatalf("reconfirm err = %v, want ErrCannotConfirm", err)
	}
	if _, err := complete.Execute(c, dto.CompleteOrderInput{OrderID: "o-a"}); !errors.Is(err, order.ErrPaymentRequired) {
		t.Fatalf("complete after the switch went back err = %v, want ErrPaymentRequired", err)
	}
	// An order confirmed under the new setting is not held back.
	if out, err := off.Execute(c, dto.ConfirmOrderInput{OrderID: "o-b"}); err != nil || out.PaymentRequired {
		t.Fatalf("confirm under default = %+v, %v", out, err)
	}
	if out, err := complete.Execute(c, dto.CompleteOrderInput{OrderID: "o-b"}); err != nil || out.Status != "COMPLETED" {
		t.Fatalf("complete o-2 = %+v, %v", out, err)
	}
}
