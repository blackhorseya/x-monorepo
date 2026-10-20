package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
	"connectrpc.com/connect"

	inventoryv1 "github.com/blackhorseya/x-monorepo/pkgs/inventory/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/contract/v1/inventoryv1connect"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1/orderingv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	commonv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/common/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// End-to-end through the wired graph: proto client → rpc adapter → decorated
// usecase → domain → infra.
func TestOrderService_E2E(t *testing.T) {
	srv, cleanup, err := Initialize(&config.AppConfig{
		HTTP: config.HTTP{Addr: ":0"},
		Log:  config.Log{Level: "error", Format: "text"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Probes: alive, and ready (in-memory repositories: no dependency to check).
	for _, p := range []string{server.HealthzPath, server.ReadyzPath} {
		res, err := http.Get(ts.URL + p)
		if err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %v, %v", p, res, err)
		}
		_ = res.Body.Close()
	}

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
		// Shape errors are rejected by the protovalidate interceptor, before the
		// use case: same code, the shared validation x-error-code.
		{"invalid currency (protovalidate)", func() error {
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

	// A rejected request names the violated fields in a buf.validate.Violations detail.
	_, err = client.CreateOrder(c, connect.NewRequest(&orderingv1.CreateOrderRequest{CustomerId: ""}))
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeInvalidArgument || ce.Meta().Get("x-error-code") != server.ValidationErrorCode {
		t.Fatalf("empty request: err = %v", err)
	}
	if len(ce.Details()) != 1 {
		t.Fatalf("details = %d, want violations", len(ce.Details()))
	}
	detail, err := ce.Details()[0].Value()
	if err != nil {
		t.Fatal(err)
	}
	violations, ok := detail.(*validate.Violations)
	if !ok || len(violations.GetViolations()) != 2 { // customer_id, items
		t.Fatalf("violations = %v", detail)
	}

	// ListOrders: cursor pagination end to end (3 orders for c-2, pages of 2).
	for range 2 {
		if _, err := client.CreateOrder(c, connect.NewRequest(&orderingv1.CreateOrderRequest{
			CustomerId: "c-2",
			Items:      []*orderingv1.LineItem{{Sku: "S", Quantity: 1, UnitPrice: &commonv1.Money{AmountMinor: 1, Currency: "USD"}}},
		})); err != nil {
			t.Fatalf("CreateOrder c-2: %v", err)
		}
	}
	page1, err := client.ListOrders(c, connect.NewRequest(&orderingv1.ListOrdersRequest{PageSize: 2}))
	if err != nil || len(page1.Msg.GetOrders()) != 2 || page1.Msg.GetNextPageToken() == "" {
		t.Fatalf("ListOrders page 1: %v, %v", page1, err)
	}
	page2, err := client.ListOrders(c, connect.NewRequest(&orderingv1.ListOrdersRequest{PageSize: 2, PageToken: page1.Msg.GetNextPageToken()}))
	if err != nil || len(page2.Msg.GetOrders()) != 1 || page2.Msg.GetNextPageToken() != "" {
		t.Fatalf("ListOrders page 2: %v, %v", page2, err)
	}
	if page2.Msg.GetOrders()[0].GetId() != o.GetId() {
		t.Fatalf("oldest order last: got %s want %s", page2.Msg.GetOrders()[0].GetId(), o.GetId())
	}
	only, err := client.ListOrders(c, connect.NewRequest(&orderingv1.ListOrdersRequest{CustomerId: "c-1", Status: orderingv1.OrderStatus_ORDER_STATUS_CANCELLED}))
	if err != nil || len(only.Msg.GetOrders()) != 1 {
		t.Fatalf("ListOrders filtered: %v, %v", only, err)
	}
	_, err = client.ListOrders(c, connect.NewRequest(&orderingv1.ListOrdersRequest{CustomerId: "c-1", PageToken: page1.Msg.GetNextPageToken()}))
	if !errors.As(err, &ce) || ce.Code() != connect.CodeInvalidArgument || ce.Meta().Get("x-error-code") != "pagination.invalid_token" {
		t.Fatalf("token with other filters: err = %v", err)
	}

	// Second BC on the same server: inventory's RPC surface (reservations are
	// made by the worker, not here; in-process GoChannel has no subscriber).
	inv := inventoryv1connect.NewInventoryServiceClient(http.DefaultClient, ts.URL)
	set, err := inv.SetStock(c, connect.NewRequest(&inventoryv1.SetStockRequest{Sku: "SKU-1", OnHand: 10}))
	if err != nil || set.Msg.GetStock().GetAvailable() != 10 {
		t.Fatalf("SetStock: %v, %v", set, err)
	}
	stock, err := inv.GetStock(c, connect.NewRequest(&inventoryv1.GetStockRequest{Sku: "SKU-1"}))
	if err != nil || stock.Msg.GetStock().GetOnHand() != 10 || stock.Msg.GetStock().GetReserved() != 0 {
		t.Fatalf("GetStock: %v, %v", stock, err)
	}
	if _, err := inv.SetStock(c, connect.NewRequest(&inventoryv1.SetStockRequest{Sku: "SKU-0", OnHand: 1})); err != nil {
		t.Fatal(err)
	}
	list, err := inv.ListStocks(c, connect.NewRequest(&inventoryv1.ListStocksRequest{PageSize: 1}))
	if err != nil || len(list.Msg.GetStocks()) != 1 || list.Msg.GetStocks()[0].GetSku() != "SKU-0" || list.Msg.GetNextPageToken() == "" {
		t.Fatalf("ListStocks page 1: %v, %v", list, err)
	}
	list, err = inv.ListStocks(c, connect.NewRequest(&inventoryv1.ListStocksRequest{PageSize: 1, PageToken: list.Msg.GetNextPageToken()}))
	if err != nil || len(list.Msg.GetStocks()) != 1 || list.Msg.GetStocks()[0].GetSku() != "SKU-1" || list.Msg.GetNextPageToken() != "" {
		t.Fatalf("ListStocks page 2: %v, %v", list, err)
	}
	_, err = inv.GetReservation(c, connect.NewRequest(&inventoryv1.GetReservationRequest{OrderId: o.GetId()}))
	if !errors.As(err, &ce) || ce.Code() != connect.CodeNotFound || ce.Meta().Get("x-error-code") != "reservation.not_found" {
		t.Fatalf("GetReservation without a worker: err = %v", err)
	}
	_, err = inv.SetStock(c, connect.NewRequest(&inventoryv1.SetStockRequest{Sku: "SKU-1", OnHand: -1}))
	if !errors.As(err, &ce) || ce.Code() != connect.CodeInvalidArgument || ce.Meta().Get("x-error-code") != server.ValidationErrorCode {
		t.Fatalf("SetStock negative (protovalidate): err = %v", err)
	}

	// Metrics decorator is wired for every use case and exposed on /api/metrics.
	res, err := http.Get(ts.URL + server.MetricsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var sb strings.Builder
	if _, err := io.Copy(&sb, res.Body); err != nil {
		t.Fatal(err)
	}
	body := sb.String()
	for _, want := range []string{
		`usecase_duration_seconds_count{kind="ok",usecase="ordering.create_order"} 3`,
		`usecase_duration_seconds_count{kind="ok",usecase="ordering.cancel_order"} 1`,
		`usecase_duration_seconds_count{kind="failed_precondition",usecase="ordering.cancel_order"} 1`,
		`usecase_duration_seconds_count{kind="not_found",usecase="ordering.get_order"} 1`,
		`usecase_duration_seconds_count{kind="ok",usecase="ordering.list_orders"} 3`,
		`usecase_duration_seconds_count{kind="invalid_argument",usecase="ordering.list_orders"} 1`,
		`usecase_duration_seconds_count{kind="ok",usecase="inventory.set_stock"} 2`,
		`usecase_duration_seconds_count{kind="ok",usecase="inventory.list_stocks"} 2`,
		`usecase_duration_seconds_count{kind="ok",usecase="inventory.get_stock"} 1`,
		`usecase_duration_seconds_count{kind="not_found",usecase="inventory.get_reservation"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("%s missing %q\n%s", server.MetricsPath, want, body)
		}
	}
}

// gRPC needs HTTP/2 and gRPC-Web is a distinct wire format; both must survive the
// router (gin wildcard routes + h2c) exactly like the Connect protocol does.
func TestOrderService_Protocols(t *testing.T) {
	srv, cleanup, err := Initialize(&config.AppConfig{
		HTTP: config.HTTP{Addr: ":0"},
		Log:  config.Log{Level: "error", Format: "text"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	ts := httptest.NewUnstartedServer(srv.Handler())
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	ts.Config.Protocols = protocols
	ts.Start()
	defer ts.Close()

	h2c := new(http.Protocols)
	h2c.SetUnencryptedHTTP2(true)
	h2cClient := &http.Client{Transport: &http.Transport{Protocols: h2c}}

	// Every protocol works at the bare protocol path and under server.RPCPrefix
	// (a connect-go client prefixes gRPC too; grpc-go could not, hence both).
	for _, tt := range []struct {
		name   string
		client *http.Client
		opts   []connect.ClientOption
	}{
		{"connect", http.DefaultClient, nil},
		{"grpc", h2cClient, []connect.ClientOption{connect.WithGRPC()}},
		{"grpc-web", http.DefaultClient, []connect.ClientOption{connect.WithGRPCWeb()}},
		{"connect/api", http.DefaultClient, nil},
		{"grpc/api", h2cClient, []connect.ClientOption{connect.WithGRPC()}},
		{"grpc-web/api", http.DefaultClient, []connect.ClientOption{connect.WithGRPCWeb()}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base := ts.URL
			if strings.HasSuffix(tt.name, "/api") {
				base += server.RPCPrefix
			}
			client := orderingv1connect.NewOrderServiceClient(tt.client, base, tt.opts...)
			c := context.Background()

			// Response meta travels as headers on every protocol, on errors too.
			_, err := client.GetOrder(c, connect.NewRequest(&orderingv1.GetOrderRequest{Id: "nope"}))
			var ce *connect.Error
			if !errors.As(err, &ce) || ce.Code() != connect.CodeNotFound {
				t.Fatalf("err = %v, want NotFound", err)
			}
			if ce.Meta().Get(server.RequestIDHeader) == "" {
				t.Fatalf("error without %s: %v", server.RequestIDHeader, ce.Meta())
			}

			req := connect.NewRequest(&orderingv1.CreateOrderRequest{
				CustomerId: "c-1",
				Items:      []*orderingv1.LineItem{{Sku: "S", Quantity: 1, UnitPrice: &commonv1.Money{AmountMinor: 1, Currency: "USD"}}},
			})
			id := "client-" + strings.ReplaceAll(tt.name, "/", "-")
			req.Header().Set(server.RequestIDHeader, id)
			res, err := client.CreateOrder(c, req)
			if err != nil {
				t.Fatal(err)
			}
			if got := res.Header().Get(server.RequestIDHeader); got != id {
				t.Fatalf("%s = %q, want echo of the client's id", server.RequestIDHeader, got)
			}
		})
	}
}

// Docs are mounted under /api only when enabled; disabled answers with a hint.
func TestDocs(t *testing.T) {
	get := func(t *testing.T, h http.Handler, path string) (int, string) {
		t.Helper()
		ts := httptest.NewServer(h)
		defer ts.Close()
		res, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		var sb strings.Builder
		_, _ = io.Copy(&sb, res.Body)
		return res.StatusCode, sb.String()
	}
	newServer := func(t *testing.T, docs bool) http.Handler {
		t.Helper()
		srv, cleanup, err := Initialize(&config.AppConfig{
			HTTP: config.HTTP{Addr: ":0", Docs: docs},
			Log:  config.Log{Level: "error", Format: "text"},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cleanup)
		return srv.Handler()
	}

	on := newServer(t, true)
	for _, path := range []string{
		server.DocsPath,
		server.DocsPath + "/ordering", server.OpenAPIPath + "/ordering.yaml",
		server.DocsPath + "/inventory", server.OpenAPIPath + "/inventory.yaml",
	} {
		if code, _ := get(t, on, path); code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, code)
		}
	}
	if code, body := get(t, on, server.OpenAPIPath+"/ordering.yaml"); code != 200 || !strings.Contains(body, "openapi: 3") {
		t.Fatalf("openapi body: %d %q", code, body[:min(len(body), 40)])
	}
	if code, _ := get(t, on, "/docs"); code != http.StatusNotFound {
		t.Fatalf("unprefixed /docs = %d, want 404", code)
	}

	off := newServer(t, false)
	if code, body := get(t, off, server.DocsPath); code != http.StatusNotFound || !strings.Contains(body, "APP_HTTP_DOCS=true") {
		t.Fatalf("disabled docs = %d %q", code, body)
	}
	if code, _ := get(t, off, server.OpenAPIPath+"/ordering.yaml"); code != http.StatusNotFound {
		t.Fatalf("disabled openapi = %d, want 404", code)
	}
}
