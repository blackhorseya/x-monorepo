package main

import (
	"context"
	"errors"
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

// CreateOrder.idempotency_key through the wired server: the proto rules, the
// Connect code of a reused key, and the scope (a customer's own keys).
func TestOrdering_IdempotencyKey(t *testing.T) {
	srv, cleanup, err := Initialize(&config.AppConfig{
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
	line := func(sku string) []*orderingv1.LineItem {
		return []*orderingv1.LineItem{{Sku: sku, Quantity: 1, UnitPrice: &commonv1.Money{AmountMinor: 100, Currency: "USD"}}}
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
