package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1/orderingv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	orderingpg "github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
)

// Through the whole wired apps/rpc on Postgres (default config: nothing about payment is set), an
// order whose stored snapshot says payment is required is not completable over the wire —
// FAILED_PRECONDITION with x-error-code order.payment_required, the order unchanged — while an
// order whose snapshot is false completes. apps/rpc has no setting of its own for this: the
// snapshot is the only input, so it holds whatever rpc's config says (docs/payment.md §8). The
// snapshots are planted the way the worker's ConfirmOrder stores them (the worker half is
// apps/worker's TestWorker_RequirePaymentSnapshot_RealEntry).
func TestOrdering_CompleteOrder_PaymentRequiredSnapshot(t *testing.T) {
	s := newCatalogStockServer(t)
	c := context.Background()
	repo := orderingpg.NewOrderRepository(s.db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	usd, _ := kernel.NewMoney(250, "USD")
	li, err := order.NewLineItem("SKU-1", 2, usd)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[bool]string{true: "pay-rpc-required-" + now.Format("150405.000000"), false: "pay-rpc-plain-" + now.Format("150405.000000")}
	t.Cleanup(func() { // while the pool is open (registered after the server's cleanups, so it runs first)
		for _, id := range ids {
			if err := s.db.WithContext(c).Exec(`DELETE FROM ordering.orders WHERE id = ?`, id).Error; err != nil {
				t.Errorf("cleanup order %s: %v", id, err)
			}
		}
	})
	for required, id := range ids {
		o, err := order.New(order.Params{ID: order.ID(id), CustomerID: "c-pay", Items: []order.LineItem{li}, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		if err := o.Confirm(now, required); err != nil {
			t.Fatal(err)
		}
		if err := repo.Save(c, o); err != nil {
			t.Fatal(err)
		}
	}

	cl := orderingv1connect.NewOrderServiceClient(http.DefaultClient, s.url)
	_, err = cl.CompleteOrder(c, connect.NewRequest(&orderingv1.CompleteOrderRequest{Id: ids[true]}))
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeFailedPrecondition || ce.Meta().Get("x-error-code") != "order.payment_required" {
		t.Fatalf("CompleteOrder(payment_required=true) err = %v, want FAILED_PRECONDITION / order.payment_required", err)
	}
	got, err := cl.GetOrder(c, connect.NewRequest(&orderingv1.GetOrderRequest{Id: ids[true]}))
	if err != nil || got.Msg.GetOrder().GetStatus() != orderingv1.OrderStatus_ORDER_STATUS_CONFIRMED || !got.Msg.GetOrder().GetPaymentRequired() {
		t.Fatalf("after the refusal: %v, %v; want CONFIRMED with payment_required", got, err)
	}

	done, err := cl.CompleteOrder(c, connect.NewRequest(&orderingv1.CompleteOrderRequest{Id: ids[false]}))
	if err != nil || done.Msg.GetOrder().GetStatus() != orderingv1.OrderStatus_ORDER_STATUS_COMPLETED || done.Msg.GetOrder().GetPaymentRequired() {
		t.Fatalf("CompleteOrder(payment_required=false) = %v, %v; want COMPLETED, payment_required=false", done, err)
	}
}
