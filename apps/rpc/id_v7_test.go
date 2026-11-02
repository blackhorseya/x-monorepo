package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1/orderingv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// New ids are UUID v7, seen from the real entry: the order id CreateOrder
// returns and the X-Request-Id the server mints. A request id the client
// brings is echoed untouched, whatever it looks like. Ids stay opaque strings.
func TestIDs_NewOnesAreV7FromTheRealEntry(t *testing.T) {
	srv, cleanup, err := initialize(t, &config.AppConfig{
		HTTP: config.HTTP{Addr: ":0"},
		Log:  config.Log{Level: "error", Format: "text"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	seedProduct(t, ts.URL, "", "SKU-V7", 100, "USD")
	client := orderingv1connect.NewOrderServiceClient(http.DefaultClient, ts.URL)

	create := func(rid string) *connect.Response[orderingv1.CreateOrderResponse] {
		t.Helper()
		req := connect.NewRequest(&orderingv1.CreateOrderRequest{CustomerId: "c-v7", Items: []*orderingv1.CreateOrderItem{{Sku: "SKU-V7", Quantity: 1}}})
		if rid != "" {
			req.Header().Set(server.RequestIDHeader, rid)
		}
		res, err := client.CreateOrder(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}

		return res
	}
	isV7 := func(id string) bool {
		u, err := uuid.Parse(id)

		return err == nil && u.Version() == 7
	}

	res := create("")
	if id := res.Msg.GetOrder().GetId(); !isV7(id) {
		t.Fatalf("order id %q is not a UUID v7", id)
	}
	if rid := res.Header().Get(server.RequestIDHeader); !isV7(rid) {
		t.Fatalf("minted X-Request-Id %q is not a UUID v7", rid)
	}

	// A client's own value, even a v4 one, comes back exactly as sent.
	const v4 = "6ba7b810-9dad-41d1-80b4-00c04fd430c8"
	for _, own := range []string{v4, "client-42"} {
		if got := create(own).Header().Get(server.RequestIDHeader); got != own {
			t.Fatalf("client X-Request-Id %q came back as %q", own, got)
		}
	}
}
