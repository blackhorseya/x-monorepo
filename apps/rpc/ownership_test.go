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
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// Ordering ownership through the whole wired server (auth.mode optional, so a
// call without a credential is anonymous): a non-admin reads, lists and orders
// for themselves only, an admin orders on behalf of a customer, an anonymous
// caller keeps the pre-auth behaviour.
func TestOrdering_Ownership(t *testing.T) {
	srv, cleanup, err := Initialize(&config.AppConfig{
		HTTP: config.HTTP{Addr: ":0"},
		Log:  config.Log{Level: "error", Format: "text"},
		Auth: config.Auth{
			Mode:      config.AuthOptional,
			AdminRole: "root", // not the default: the role name must come from config
			APIKeys:   "alice=alice-key-0123456789,bob=bob-key-0123456789,ops=ops-key-0123456789abc:root,decoy=decoy-key-0123456789ab:admin",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := context.Background()

	client := func(key string) orderingv1connect.OrderServiceClient {
		var opts []connect.ClientOption
		if key != "" {
			opts = append(opts, connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
				return func(c context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
					req.Header().Set(server.APIKeyHeader, key)
					return next(c, req)
				}
			})))
		}
		return orderingv1connect.NewOrderServiceClient(http.DefaultClient, ts.URL, opts...)
	}
	seedProduct(t, ts.URL, "ops-key-0123456789abc", "SKU-1", 100, "USD")
	alice, bob, ops, decoy, anon := client("alice-key-0123456789"), client("bob-key-0123456789"), client("ops-key-0123456789abc"), client("decoy-key-0123456789ab"), client("")
	items := []*orderingv1.CreateOrderItem{{Sku: "SKU-1", Quantity: 1}}
	place := func(cl orderingv1connect.OrderServiceClient, customer string) (*orderingv1.Order, error) {
		res, err := cl.CreateOrder(c, connect.NewRequest(&orderingv1.CreateOrderRequest{CustomerId: customer, Items: items}))
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
	ids := func(res *connect.Response[orderingv1.ListOrdersResponse]) (out []string) {
		for _, o := range res.Msg.GetOrders() {
			out = append(out, o.GetCustomerId())
		}
		return out
	}

	t.Run("non-admin is confined to their own orders", func(t *testing.T) {
		mine, err := place(alice, "") // no customer_id: the caller
		if err != nil || mine.GetCustomerId() != "alice" {
			t.Fatalf("alice orders for herself = %v, %v", mine, err)
		}
		if _, err := place(alice, "alice"); err != nil {
			t.Fatalf("alice naming herself: %v", err)
		}
		_, err = place(bob, "alice")
		wantCode(t, err, connect.CodePermissionDenied, "order.permission_denied")

		_, err = bob.GetOrder(c, connect.NewRequest(&orderingv1.GetOrderRequest{Id: mine.GetId()}))
		wantCode(t, err, connect.CodeNotFound, "order.not_found")
		_, err = bob.GetOrder(c, connect.NewRequest(&orderingv1.GetOrderRequest{Id: "no-such-order"}))
		wantCode(t, err, connect.CodeNotFound, "order.not_found")
		if res, err := alice.GetOrder(c, connect.NewRequest(&orderingv1.GetOrderRequest{Id: mine.GetId()})); err != nil || res.Msg.GetOrder().GetId() != mine.GetId() {
			t.Fatalf("alice reads her own = %v, %v", res, err)
		}

		_, err = bob.ListOrders(c, connect.NewRequest(&orderingv1.ListOrdersRequest{CustomerId: "alice"}))
		wantCode(t, err, connect.CodePermissionDenied, "order.permission_denied")
		res, err := bob.ListOrders(c, connect.NewRequest(&orderingv1.ListOrdersRequest{}))
		if err != nil || len(res.Msg.GetOrders()) != 0 {
			t.Fatalf("bob lists %v (err %v), want none of alice's", ids(res), err)
		}
		if res, err = alice.ListOrders(c, connect.NewRequest(&orderingv1.ListOrdersRequest{})); err != nil || len(res.Msg.GetOrders()) != 2 {
			t.Fatalf("alice lists %v (err %v), want her 2", ids(res), err)
		}
	})

	t.Run("admin orders on behalf of a customer", func(t *testing.T) {
		for _, tc := range []struct{ customer, want string }{{"walk-in-9", "walk-in-9"}, {"", "ops"}} {
			got, err := place(ops, tc.customer)
			if err != nil || got.GetCustomerId() != tc.want {
				t.Fatalf("ops orders for %q = %v, %v, want customer %q", tc.customer, got, err, tc.want)
			}
		}
		res, err := ops.ListOrders(c, connect.NewRequest(&orderingv1.ListOrdersRequest{}))
		if err != nil || len(res.Msg.GetOrders()) < 4 {
			t.Fatalf("ops lists %v (err %v), want everything", ids(res), err)
		}
		// The role is whatever auth.admin_role says: "admin" is just another role here.
		_, err = place(decoy, "alice")
		wantCode(t, err, connect.CodePermissionDenied, "order.permission_denied")
	})

	t.Run("anonymous keeps the existing behaviour", func(t *testing.T) {
		_, err := place(anon, "")
		wantCode(t, err, connect.CodeInvalidArgument, "order.empty_customer")
		got, err := place(anon, "c-anon")
		if err != nil || got.GetCustomerId() != "c-anon" {
			t.Fatalf("anonymous orders for c-anon = %v, %v", got, err)
		}
		if _, err := anon.GetOrder(c, connect.NewRequest(&orderingv1.GetOrderRequest{Id: got.GetId()})); err != nil {
			t.Fatalf("anonymous GetOrder: %v", err)
		}
		res, err := anon.ListOrders(c, connect.NewRequest(&orderingv1.ListOrdersRequest{CustomerId: "alice"}))
		if err != nil || len(res.Msg.GetOrders()) != 2 {
			t.Fatalf("anonymous lists alice's %v (err %v), want 2", ids(res), err)
		}
	})
}
