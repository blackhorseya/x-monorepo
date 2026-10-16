package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1/orderingv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	commonv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/common/v1"
)

// End-to-end through the wired graph: proto client → rpc adapter → decorated
// usecase → domain → infra.
func TestOrderService_E2E(t *testing.T) {
	srv, err := Initialize(&config.AppConfig{
		HTTP: config.HTTP{Addr: ":0"},
		Log:  config.Log{Level: "error", Format: "text"},
	})
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := orderingv1connect.NewOrderServiceClient(http.DefaultClient, ts.URL)
	c := context.Background()

	created, err := client.CreateOrder(c, connect.NewRequest(&orderingv1.CreateOrderRequest{
		CustomerId: "c-1",
		Items: []*orderingv1.LineItem{
			{Sku: "SKU-1", Quantity: 2, UnitPrice: &commonv1.Money{AmountMinor: 1299, Currency: "USD"}},
		},
	}))
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	o := created.Msg.GetOrder()
	if o.GetTotal().GetAmountMinor() != 2598 || o.GetStatus() != orderingv1.OrderStatus_ORDER_STATUS_PENDING {
		t.Fatalf("unexpected order: %v", o)
	}

	cancelled, err := client.CancelOrder(c, connect.NewRequest(&orderingv1.CancelOrderRequest{Id: o.GetId()}))
	if err != nil || cancelled.Msg.GetOrder().GetStatus() != orderingv1.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatalf("CancelOrder: %v, %v", cancelled, err)
	}

	tests := []struct {
		name string
		call func() error
		want connect.Code
	}{
		{"cancel twice", func() error {
			_, err := client.CancelOrder(c, connect.NewRequest(&orderingv1.CancelOrderRequest{Id: o.GetId()}))
			return err
		}, connect.CodeFailedPrecondition},
		{"not found", func() error {
			_, err := client.GetOrder(c, connect.NewRequest(&orderingv1.GetOrderRequest{Id: "nope"}))
			return err
		}, connect.CodeNotFound},
		{"invalid currency", func() error {
			_, err := client.CreateOrder(c, connect.NewRequest(&orderingv1.CreateOrderRequest{
				CustomerId: "c-1",
				Items:      []*orderingv1.LineItem{{Sku: "S", Quantity: 1, UnitPrice: &commonv1.Money{Currency: "usd"}}},
			}))
			return err
		}, connect.CodeInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ce *connect.Error
			if err := tt.call(); !errors.As(err, &ce) || ce.Code() != tt.want {
				t.Fatalf("err = %v, want code %v", err, tt.want)
			}
			if ce.Meta().Get("x-error-code") == "" {
				t.Fatal("missing x-error-code header")
			}
		})
	}
}
