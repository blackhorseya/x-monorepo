package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	inventoryv1 "github.com/blackhorseya/x-monorepo/pkgs/inventory/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/contract/v1/inventoryv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// SetStock overwrites a SKU's on-hand, so it takes the admin role like
// ExpireReservations (the two must stay in step): with auth on a role-less
// credential is PERMISSION_DENIED, no credential is UNAUTHENTICATED, the
// admin role is whatever auth.admin_role says; with auth off it is open.
func TestInventory_SetStockAdminRole(t *testing.T) {
	start := func(t *testing.T, auth config.Auth) (client func(key string) inventoryv1connect.InventoryServiceClient, baseURL string) {
		t.Helper()
		srv, cleanup, err := initialize(t, &config.AppConfig{
			HTTP: config.HTTP{Addr: ":0"},
			Log:  config.Log{Level: "error", Format: "text"},
			Auth: auth,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cleanup)
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)

		return func(key string) inventoryv1connect.InventoryServiceClient {
			var opts []connect.ClientOption
			if key != "" {
				opts = append(opts, connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
					return func(c context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
						req.Header().Set(server.APIKeyHeader, key)
						return next(c, req)
					}
				})))
			}

			return inventoryv1connect.NewInventoryServiceClient(http.DefaultClient, ts.URL, opts...)
		}, ts.URL
	}
	wantCode := func(t *testing.T, err error, code connect.Code, errorCode string) {
		t.Helper()
		var ce *connect.Error
		if !errors.As(err, &ce) || ce.Code() != code || ce.Meta().Get("x-error-code") != errorCode {
			t.Fatalf("err = %v, want %s / %s", err, code, errorCode)
		}
	}
	c := context.Background()
	set := func(cl inventoryv1connect.InventoryServiceClient, sku string) error {
		_, err := cl.SetStock(c, connect.NewRequest(&inventoryv1.SetStockRequest{Sku: sku, OnHand: 5}))
		return err
	}
	expire := func(cl inventoryv1connect.InventoryServiceClient) error {
		_, err := cl.ExpireReservations(c, connect.NewRequest(&inventoryv1.ExpireReservationsRequest{OlderThan: durationpb.New(time.Hour), DryRun: true}))
		return err
	}

	t.Run("auth required: only the admin role may set stock", func(t *testing.T) {
		client, url := start(t, config.Auth{
			Mode:      config.AuthRequired,
			AdminRole: "root", // not the default: the role name must come from config
			APIKeys:   "plain=plain-key-0123456789ab,ops=ops-key-0123456789abc:root,decoy=decoy-key-0123456789ab:admin",
		})
		plain, ops, decoy := client("plain-key-0123456789ab"), client("ops-key-0123456789abc"), client("decoy-key-0123456789ab")
		// both SKUs are catalog products, so a refusal below is the role's, not the catalog's
		seedProduct(t, url, "ops-key-0123456789abc", "SKU-DENIED", 100, "USD")
		seedProduct(t, url, "ops-key-0123456789abc", "SKU-OPS", 100, "USD")

		for _, cl := range []inventoryv1connect.InventoryServiceClient{plain, decoy} { // role-less, and a role that is not auth.admin_role
			wantCode(t, set(cl, "SKU-DENIED"), connect.CodePermissionDenied, "auth.permission_denied")
			wantCode(t, expire(cl), connect.CodePermissionDenied, "auth.permission_denied") // same gate as SetStock
		}
		if err := set(ops, "SKU-OPS"); err != nil {
			t.Fatalf("admin SetStock: %v", err)
		}
		// the refused calls wrote nothing; the admin's did
		if _, err := ops.GetStock(c, connect.NewRequest(&inventoryv1.GetStockRequest{Sku: "SKU-DENIED"})); err == nil {
			t.Fatal("a refused SetStock created SKU-DENIED")
		}
		if res, err := plain.GetStock(c, connect.NewRequest(&inventoryv1.GetStockRequest{Sku: "SKU-OPS"})); err != nil || res.Msg.GetStock().GetOnHand() != 5 {
			t.Fatalf("role-less reads stay open: %v, %v", res, err)
		}
	})

	t.Run("auth optional: a call without a credential is unauthenticated", func(t *testing.T) {
		client, _ := start(t, config.Auth{Mode: config.AuthOptional, APIKeys: "ops=ops-key-0123456789abc:admin"})
		wantCode(t, set(client(""), "SKU-ANON"), connect.CodeUnauthenticated, "auth.unauthenticated")
	})

	t.Run("auth off: open, as ExpireReservations", func(t *testing.T) {
		client, url := start(t, config.Auth{Mode: config.AuthOff})
		seedProduct(t, url, "", "SKU-OFF", 100, "USD")
		anon := client("")
		if err := set(anon, "SKU-OFF"); err != nil {
			t.Fatalf("anonymous SetStock with auth off: %v", err)
		}
		if err := expire(anon); err != nil {
			t.Fatalf("anonymous ExpireReservations with auth off: %v", err)
		}
	})
}
