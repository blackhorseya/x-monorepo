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
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// CreateOrder.idempotency_key through the wired server: the proto rules, the
// Connect code of a reused key, and the scope (a customer's own keys).
func TestOrdering_IdempotencyKey(t *testing.T) {
	srv, cleanup, err := initialize(t, &config.AppConfig{
		HTTP: config.HTTP{Addr: ":0"},
		Log:  config.Log{Level: "error", Format: "text"},
		Auth: config.Auth{Mode: config.AuthRequired, AdminRole: "root", APIKeys: "alice=alice-key-0123456789,bob=bob-key-0123456789,ops=ops-key-0123456789abc:root"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := context.Background()

	client := func(key string) orderingv1connect.OrderServiceClient {
		return orderingv1connect.NewOrderServiceClient(http.DefaultClient, ts.URL, connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(c context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				req.Header().Set(server.APIKeyHeader, key)
				return next(c, req)
			}
		})))
	}
	alice, bob, ops := client("alice-key-0123456789"), client("bob-key-0123456789"), client("ops-key-0123456789abc")
	for _, sku := range []string{"SKU-1", "SKU-OTHER"} {
		seedProduct(t, ts.URL, "ops-key-0123456789abc", sku, 100, "USD")
	}
	line := func(sku string) []*orderingv1.CreateOrderItem {
		return []*orderingv1.CreateOrderItem{{Sku: sku, Quantity: 1}}
	}
	place := func(cl orderingv1connect.OrderServiceClient, customer, key, sku string) (*orderingv1.Order, error) {
		res, err := cl.CreateOrder(c, connect.NewRequest(&orderingv1.CreateOrderRequest{CustomerId: customer, Items: line(sku), IdempotencyKey: key}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetOrder(), nil
	}
	wantCode := func(t *testing.T, err error, code connect.Code, errorCode string) {
		t.Helper()
		var ce *connect.Error
		if !errors.As(err, &ce) || ce.Code() != code || ce.Meta().Get("x-error-code") != errorCode {
			t.Fatalf("err = %v, want %s / %s", err, code, errorCode)
		}
	}

	first, err := place(alice, "", "retry-1", "SKU-1")
	if err != nil {
		t.Fatal(err)
	}
	again, err := place(alice, "", "retry-1", "SKU-1")
	if err != nil || again.GetId() != first.GetId() {
		t.Fatalf("retry = %v, %v, want order %s", again, err, first.GetId())
	}

	_, err = place(alice, "", "retry-1", "SKU-OTHER")
	wantCode(t, err, connect.CodeAlreadyExists, "order.idempotency_key_reused")
	for _, leak := range []string{first.GetId(), "SKU-1", "SKU-OTHER", "retry-1"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error %q leaks %q", err, leak)
		}
	}

	// Another customer's use of the same key is neither a replay nor a conflict.
	theirs, err := place(bob, "", "retry-1", "SKU-OTHER")
	if err != nil || theirs.GetId() == first.GetId() || theirs.GetCustomerId() != "bob" {
		t.Fatalf("bob with alice's key = %v, %v", theirs, err)
	}
	// An admin acting for alice is in alice's scope.
	if got, err := place(ops, "alice", "retry-1", "SKU-1"); err != nil || got.GetId() != first.GetId() {
		t.Fatalf("admin for alice = %v, %v, want order %s", got, err, first.GetId())
	}

	for name, key := range map[string]string{"too long": strings.Repeat("k", 65), "space": "a b", "control": "a\tb", "non-ascii": "鍵"} {
		_, err := place(alice, "", key, "SKU-1")
		t.Run(name, func(t *testing.T) {
			wantCode(t, err, connect.CodeInvalidArgument, "request.validation_failed")
			if strings.Contains(err.Error(), key) { // R4: a rejected key is not echoed either
				t.Fatalf("validation error %q echoes the key", err)
			}
		})
	}
	if _, err := place(alice, "", strings.Repeat("k", 64), "SKU-1"); err != nil {
		t.Fatalf("64-char key: %v", err)
	}

	// No key: every call is a new order.
	a, _ := place(alice, "", "", "SKU-1")
	b, _ := place(alice, "", "", "SKU-1")
	if a == nil || b == nil || a.GetId() == b.GetId() {
		t.Fatalf("keyless calls = %v, %v, want two orders", a, b)
	}
}

// ordering_create_order_total{result}: what CreateOrder ended in, counted at the
// outermost decorator (after the transaction committed). Only the result label:
// no key, customer or order id (R4, cardinality).
func TestOrdering_CreateOrderResultMetric(t *testing.T) {
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
	c := context.Background()
	cl := orderingv1connect.NewOrderServiceClient(http.DefaultClient, ts.URL)
	for _, sku := range []string{"SKU-1", "SKU-OTHER"} {
		seedProduct(t, ts.URL, "", sku, 100, "USD")
	}

	place := func(key, sku string, qty int32) error {
		_, err := cl.CreateOrder(c, connect.NewRequest(&orderingv1.CreateOrderRequest{
			CustomerId:     "metric-customer",
			IdempotencyKey: key,
			Items:          []*orderingv1.CreateOrderItem{{Sku: sku, Quantity: qty}},
		}))
		return err
	}
	for _, step := range []struct {
		key, sku string
		qty      int32
		wantErr  bool
	}{
		{"", "SKU-1", 1, false},                // created (keyless)
		{"secret-key-1", "SKU-1", 1, false},    // created
		{"secret-key-1", "SKU-1", 1, false},    // replayed
		{"secret-key-1", "SKU-1", 1, false},    // replayed
		{"secret-key-1", "SKU-OTHER", 1, true}, // key_reused
		{"", "SKU-1", -1, true},                // validation error: not counted
	} {
		if err := place(step.key, step.sku, step.qty); (err != nil) != step.wantErr {
			t.Fatalf("place(%q, %q, %d): err = %v, wantErr %v", step.key, step.sku, step.qty, err, step.wantErr)
		}
	}

	res, err := http.Get(ts.URL + server.MetricsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var sb strings.Builder
	if _, err := io.Copy(&sb, res.Body); err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}
	for _, line := range strings.Split(sb.String(), "\n") {
		if !strings.HasPrefix(line, "ordering_create_order_total{") {
			continue
		}
		labels := line[strings.Index(line, "{")+1 : strings.LastIndex(line, "}")]
		result := ""
		for _, kv := range strings.Split(labels, ",") {
			name, val, _ := strings.Cut(kv, "=")
			switch name {
			case "result":
				result = strings.Trim(val, `"`)
			case "otel_scope_name", "otel_scope_version", "otel_scope_schema_url", "otel_scope_info", "job", "instance":
			default:
				t.Fatalf("unexpected label %q on %s: only result may vary", name, line)
			}
		}
		got[result] = line[strings.LastIndex(line, " ")+1:]
	}
	want := map[string]string{"created": "2", "replayed": "2", "key_reused": "1"}
	if len(got) != len(want) {
		t.Fatalf("ordering_create_order_total results = %v, want %v\n%s", got, want, sb.String())
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("ordering_create_order_total{result=%q} = %q, want %s (all: %v)", k, got[k], v, got)
		}
	}
	for _, leak := range []string{"secret-key-1", "metric-customer"} {
		if strings.Contains(sb.String(), leak) {
			t.Fatalf("/api/metrics leaks %q", leak)
		}
	}
}
