package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

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

	// Metrics decorator is wired for every use case and exposed on /metrics.
	res, err := http.Get(ts.URL + "/metrics")
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
		`usecase_duration_seconds_count{kind="ok",usecase="ordering.create_order"} 1`,
		`usecase_duration_seconds_count{kind="ok",usecase="ordering.cancel_order"} 1`,
		`usecase_duration_seconds_count{kind="failed_precondition",usecase="ordering.cancel_order"} 1`,
		`usecase_duration_seconds_count{kind="not_found",usecase="ordering.get_order"} 1`,
		`usecase_duration_seconds_count{kind="invalid_argument",usecase="ordering.create_order"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/metrics missing %q\n%s", want, body)
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

	for _, tt := range []struct {
		name   string
		client *http.Client
		opt    connect.ClientOption
	}{
		{"grpc", h2cClient, connect.WithGRPC()},
		{"grpc-web", http.DefaultClient, connect.WithGRPCWeb()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := orderingv1connect.NewOrderServiceClient(tt.client, ts.URL, tt.opt)
			_, err := client.GetOrder(context.Background(), connect.NewRequest(&orderingv1.GetOrderRequest{Id: "nope"}))
			var ce *connect.Error
			if !errors.As(err, &ce) || ce.Code() != connect.CodeNotFound {
				t.Fatalf("err = %v, want NotFound", err)
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
	for _, path := range []string{server.DocsPath, server.DocsPath + "/ordering", server.OpenAPIPath + "/ordering.yaml"} {
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
