package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	catalogv1 "github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1/catalogv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	commonv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/common/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

func startCatalog(t *testing.T, auth config.Auth) func(key string) catalogv1connect.CatalogServiceClient {
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

	return func(key string) catalogv1connect.CatalogServiceClient {
		var opts []connect.ClientOption
		if key != "" {
			opts = append(opts, connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
				return func(c context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
					req.Header().Set(server.APIKeyHeader, key)
					return next(c, req)
				}
			})))
		}

		return catalogv1connect.NewCatalogServiceClient(http.DefaultClient, ts.URL, opts...)
	}
}

func wantConnectCode(t *testing.T, err error, code connect.Code, errorCode string) {
	t.Helper()
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != code || ce.Meta().Get("x-error-code") != errorCode {
		t.Fatalf("err = %v, want %s / %s", err, code, errorCode)
	}
}

func upsertReq(sku string, amount int64, currency string) *connect.Request[catalogv1.UpsertProductRequest] {
	return connect.NewRequest(&catalogv1.UpsertProductRequest{Sku: sku, Name: "Name " + sku, Price: &commonv1.Money{AmountMinor: amount, Currency: currency}})
}

// UpsertProduct and DeactivateProduct change what customers will be charged,
// so with auth on they take the admin role like SetStock: a role-less
// credential is PERMISSION_DENIED, no credential is UNAUTHENTICATED, the admin
// role is whatever auth.admin_role says; the reads stay open; auth off is open.
func TestCatalog_WritesNeedTheAdminRole(t *testing.T) {
	c := context.Background()

	t.Run("auth required", func(t *testing.T) {
		client := startCatalog(t, config.Auth{
			Mode:      config.AuthRequired,
			AdminRole: "root", // not the default: the role name must come from config
			APIKeys:   "plain=plain-key-0123456789ab,ops=ops-key-0123456789abc:root,decoy=decoy-key-0123456789ab:admin",
		})
		plain, ops, decoy := client("plain-key-0123456789ab"), client("ops-key-0123456789abc"), client("decoy-key-0123456789ab")

		if _, err := ops.UpsertProduct(c, upsertReq("SKU-OPS", 100, "USD")); err != nil {
			t.Fatalf("admin UpsertProduct: %v", err)
		}
		for _, cl := range []catalogv1connect.CatalogServiceClient{plain, decoy} { // role-less, and a role that is not auth.admin_role
			_, err := cl.UpsertProduct(c, upsertReq("SKU-DENIED", 100, "USD"))
			wantConnectCode(t, err, connect.CodePermissionDenied, "auth.permission_denied")
			_, err = cl.UpsertProduct(c, upsertReq("SKU-OPS", 1, "USD")) // reprice of an existing product
			wantConnectCode(t, err, connect.CodePermissionDenied, "auth.permission_denied")
			_, err = cl.DeactivateProduct(c, connect.NewRequest(&catalogv1.DeactivateProductRequest{Sku: "SKU-OPS"}))
			wantConnectCode(t, err, connect.CodePermissionDenied, "auth.permission_denied")
		}
		_, err := client("").UpsertProduct(c, upsertReq("SKU-ANON", 100, "USD"))
		wantConnectCode(t, err, connect.CodeUnauthenticated, "auth.unauthenticated")

		// the refused calls wrote nothing; the admin's did; role-less reads stay open
		if _, err := ops.GetProduct(c, connect.NewRequest(&catalogv1.GetProductRequest{Sku: "SKU-DENIED"})); err == nil {
			t.Fatal("a refused UpsertProduct created SKU-DENIED")
		}
		res, err := plain.GetProduct(c, connect.NewRequest(&catalogv1.GetProductRequest{Sku: "SKU-OPS"}))
		if err != nil || res.Msg.GetProduct().GetPrice().GetAmountMinor() != 100 || !res.Msg.GetProduct().GetActive() {
			t.Fatalf("role-less read: %v, %v (the denied reprice and deactivate must not have applied)", res, err)
		}
		if _, err := plain.ListProducts(c, connect.NewRequest(&catalogv1.ListProductsRequest{})); err != nil {
			t.Fatalf("role-less list: %v", err)
		}
	})

	t.Run("auth off", func(t *testing.T) {
		anon := startCatalog(t, config.Auth{Mode: config.AuthOff})("")
		if _, err := anon.UpsertProduct(c, upsertReq("SKU-OFF", 100, "USD")); err != nil {
			t.Fatalf("anonymous UpsertProduct with auth off: %v", err)
		}
		if _, err := anon.DeactivateProduct(c, connect.NewRequest(&catalogv1.DeactivateProductRequest{Sku: "SKU-OFF"})); err != nil {
			t.Fatalf("anonymous DeactivateProduct with auth off: %v", err)
		}
	})
}

// Through the real server: validation, domain errors and the read-your-write
// contract (a reprice is what Get returns next).
func TestCatalog_RoundTrip(t *testing.T) {
	c := context.Background()
	cl := startCatalog(t, config.Auth{Mode: config.AuthOff})("")

	if _, err := cl.GetProduct(c, connect.NewRequest(&catalogv1.GetProductRequest{Sku: "A"})); err == nil {
		t.Fatal("unknown product found")
	} else {
		wantConnectCode(t, err, connect.CodeNotFound, "product.not_found")
	}
	if _, err := cl.UpsertProduct(c, upsertReq("A", 150, "USD")); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.UpsertProduct(c, upsertReq("A", 200, "USD")); err != nil {
		t.Fatal(err)
	}
	got, err := cl.GetProduct(c, connect.NewRequest(&catalogv1.GetProductRequest{Sku: "A"}))
	if err != nil || got.Msg.GetProduct().GetPrice().GetAmountMinor() != 200 {
		t.Fatalf("Get after reprice = %v, %v", got, err)
	}

	// request shape is the interceptor's: negative amount, lower case currency
	_, err = cl.UpsertProduct(c, upsertReq("B", -1, "USD"))
	wantConnectCode(t, err, connect.CodeInvalidArgument, "request.validation_failed")
	_, err = cl.UpsertProduct(c, upsertReq("B", 1, "usd"))
	wantConnectCode(t, err, connect.CodeInvalidArgument, "request.validation_failed")
	_, err = cl.UpsertProduct(c, connect.NewRequest(&catalogv1.UpsertProductRequest{Sku: "B", Name: "B"}))
	wantConnectCode(t, err, connect.CodeInvalidArgument, "request.validation_failed")

	if _, err = cl.DeactivateProduct(c, connect.NewRequest(&catalogv1.DeactivateProductRequest{Sku: "A"})); err != nil {
		t.Fatal(err)
	}
	list, err := cl.ListProducts(c, connect.NewRequest(&catalogv1.ListProductsRequest{SkuPrefix: "A"}))
	if err != nil || len(list.Msg.GetProducts()) != 1 || list.Msg.GetProducts()[0].GetActive() {
		t.Fatalf("List = %v, %v; want the inactive product listed with active=false", list, err)
	}
	_, err = cl.DeactivateProduct(c, connect.NewRequest(&catalogv1.DeactivateProductRequest{Sku: "NOPE"}))
	wantConnectCode(t, err, connect.CodeNotFound, "product.not_found")
}

// ISO 4217 means an assigned code: three capital letters that no currency uses
// pass protovalidate but are refused by the catalog.
func TestCatalog_UnassignedCurrency(t *testing.T) {
	c := context.Background()
	cl := startCatalog(t, config.Auth{Mode: config.AuthOff})("")

	_, err := cl.UpsertProduct(c, upsertReq("A", 100, "ZZZ"))
	wantConnectCode(t, err, connect.CodeInvalidArgument, "product.unsupported_currency")
	if _, err = cl.GetProduct(c, connect.NewRequest(&catalogv1.GetProductRequest{Sku: "A"})); err == nil {
		t.Fatal("a refused upsert created the product")
	}
	if _, err = cl.UpsertProduct(c, upsertReq("A", 100, "TWD")); err != nil {
		t.Fatalf("assigned currency: %v", err)
	}
}

func TestCatalog_QuotePrices(t *testing.T) {
	c := context.Background()
	cl := startCatalog(t, config.Auth{Mode: config.AuthOff})("")
	for sku, price := range map[string]int64{"A": 150, "B": 90, "OLD": 5} {
		if _, err := cl.UpsertProduct(c, upsertReq(sku, price, "USD")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cl.DeactivateProduct(c, connect.NewRequest(&catalogv1.DeactivateProductRequest{Sku: "OLD"})); err != nil {
		t.Fatal(err)
	}

	res, err := cl.QuotePrices(c, connect.NewRequest(&catalogv1.QuotePricesRequest{Skus: []string{"B", "NOPE", "A", "OLD", "A", "GONE"}}))
	if err != nil {
		t.Fatal(err)
	}
	m := res.Msg
	if len(m.GetPrices()) != 2 || m.GetPrices()["A"].GetAmountMinor() != 150 || m.GetPrices()["B"].GetCurrency() != "USD" {
		t.Fatalf("prices = %v", m.GetPrices())
	}
	if got := m.GetMissing(); len(got) != 2 || got[0] != "NOPE" || got[1] != "GONE" {
		t.Fatalf("missing = %v, want [NOPE GONE]", got)
	}
	if got := m.GetInactive(); len(got) != 1 || got[0] != "OLD" {
		t.Fatalf("inactive = %v, want [OLD]", got)
	}

	// request shape: 1 to 100 SKUs, none empty
	_, err = cl.QuotePrices(c, connect.NewRequest(&catalogv1.QuotePricesRequest{}))
	wantConnectCode(t, err, connect.CodeInvalidArgument, "request.validation_failed")
	_, err = cl.QuotePrices(c, connect.NewRequest(&catalogv1.QuotePricesRequest{Skus: []string{"A", ""}}))
	wantConnectCode(t, err, connect.CodeInvalidArgument, "request.validation_failed")
	many := make([]string, 101)
	for i := range many {
		many[i] = "A"
	}
	_, err = cl.QuotePrices(c, connect.NewRequest(&catalogv1.QuotePricesRequest{Skus: many}))
	wantConnectCode(t, err, connect.CodeInvalidArgument, "request.validation_failed")
}

// Reads stay open: a credential without the admin role may ask what things cost.
func TestCatalog_QuotePricesIsARead(t *testing.T) {
	client := startCatalog(t, config.Auth{Mode: config.AuthRequired, APIKeys: "plain=plain-key-0123456789ab,ops=ops-key-0123456789abc:admin"})
	if _, err := client("ops-key-0123456789abc").UpsertProduct(context.Background(), upsertReq("A", 150, "USD")); err != nil {
		t.Fatal(err)
	}
	res, err := client("plain-key-0123456789ab").QuotePrices(context.Background(), connect.NewRequest(&catalogv1.QuotePricesRequest{Skus: []string{"A"}}))
	if err != nil || res.Msg.GetPrices()["A"].GetAmountMinor() != 150 {
		t.Fatalf("role-less quote = %v, %v", res, err)
	}
	_, err = client("").QuotePrices(context.Background(), connect.NewRequest(&catalogv1.QuotePricesRequest{Skus: []string{"A"}}))
	wantConnectCode(t, err, connect.CodeUnauthenticated, "auth.unauthenticated")
}
