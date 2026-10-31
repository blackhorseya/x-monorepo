package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	catalogv1 "github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1/catalogv1connect"
	catalogpg "github.com/blackhorseya/x-monorepo/pkgs/catalog/infra/postgres"
	inventoryv1 "github.com/blackhorseya/x-monorepo/pkgs/inventory/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/contract/v1/inventoryv1connect"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1/orderingv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	commonv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/common/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// The catalog is its own process now: these tests run apps/rpc against the real catalog-rpc binary
// over a real socket (and a real Postgres where the catalog has to survive a restart).

// catalogOnPostgres is a catalog-rpc whose products live in Postgres, empty at the start.
func catalogOnPostgres(t *testing.T) *catalogProc {
	t.Helper()
	cfg := config.Postgres{URL: pgtest.URL(t)}
	c := context.Background()
	if err := postgres.Up(c, cfg, catalogpg.Migration()); err != nil {
		t.Fatal(err)
	}
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	db, closeDB, err := postgres.NewDB(cfg, tel)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	if err := db.WithContext(c).Exec("TRUNCATE catalog.products").Error; err != nil {
		t.Fatal(err)
	}

	return startCatalogProc(t, "APP_POSTGRES_URL="+cfg.URL)
}

func rpcServer(t *testing.T, cfg *config.AppConfig) *httptest.Server {
	t.Helper()
	cfg.HTTP.Addr = ":0"
	cfg.Log = config.Log{Level: "error", Format: "text"}
	srv, cleanup, err := initialize(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return ts
}

func wantCode(t *testing.T, err error, code connect.Code, errorCode string) {
	t.Helper()
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != code || ce.Meta().Get("x-error-code") != errorCode {
		t.Fatalf("err = %v, want %s / %s", err, code, errorCode)
	}
}

// rpc has no catalog of its own: without an upstream and the service key it does not start. There is
// no in-process fallback to quietly fall back on, and the refusal never prints the key.
func TestRPC_RefusesToStartWithoutACatalogUpstream(t *testing.T) {
	const key = "a-service-key-0123456789abcdef"
	for name, cat := range map[string]config.Catalog{
		"nothing":      {},
		"no key":       {Upstream: "http://catalog-rpc:80", Timeout: time.Second},
		"no upstream":  {APIKey: key, Timeout: time.Second},
		"not a URL":    {Upstream: "catalog-rpc", APIKey: key, Timeout: time.Second},
		"zero timeout": {Upstream: "http://catalog-rpc:80", APIKey: key},
	} {
		t.Run(name, func(t *testing.T) {
			srv, cleanup, err := Initialize(&config.AppConfig{
				HTTP: config.HTTP{Addr: ":0"}, Log: config.Log{Level: "error", Format: "text"}, Catalog: cat,
			})
			if err == nil {
				cleanup()
				_ = srv
				t.Fatal("rpc started without a usable catalog upstream")
			}
			if strings.Contains(err.Error(), key) {
				t.Fatalf("the refusal carries the key: %v", err)
			}
		})
	}
}

// The catalog is reached over the network: with nothing listening upstream, catalog.v1 on rpc is
// UNAVAILABLE (it used to answer from memory inside the process).
func TestRPC_CatalogLivesBehindTheNetwork(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	_ = ln.Close()

	ts := rpcServer(t, &config.AppConfig{Catalog: config.Catalog{Upstream: dead, APIKey: serviceKey, Timeout: time.Second}})
	cl := catalogv1connect.NewCatalogServiceClient(http.DefaultClient, ts.URL)
	_, err = cl.UpsertProduct(context.Background(), connect.NewRequest(&catalogv1.UpsertProductRequest{
		Sku: "NET-1", Name: "n", Price: &commonv1.Money{AmountMinor: 1, Currency: "USD"},
	}))
	wantCode(t, err, connect.CodeUnavailable, "catalog.unavailable")
	_, err = cl.GetProduct(context.Background(), connect.NewRequest(&catalogv1.GetProductRequest{Sku: "NET-1"}))
	wantCode(t, err, connect.CodeUnavailable, "catalog.unavailable")
}

// CreateOrder and SetStock fail closed while the catalog is down, write nothing, leave a replay
// answerable, leave every other API alone, and recover by themselves when the catalog comes back.
func TestCatalogOutage_FailsClosedAndRecovers(t *testing.T) {
	cat := catalogOnPostgres(t)
	ts := rpcServer(t, withCatalog(&config.AppConfig{}, cat))
	c := context.Background()
	orders := orderingv1connect.NewOrderServiceClient(http.DefaultClient, ts.URL)
	inv := inventoryv1connect.NewInventoryServiceClient(http.DefaultClient, ts.URL)

	seedProduct(t, ts.URL, "", "OUT-1", 100, "USD")
	if _, err := inv.SetStock(c, connect.NewRequest(&inventoryv1.SetStockRequest{Sku: "OUT-1", OnHand: 10})); err != nil {
		t.Fatal(err)
	}
	create := func(key string) (*connect.Response[orderingv1.CreateOrderResponse], error) {
		return orders.CreateOrder(c, connect.NewRequest(&orderingv1.CreateOrderRequest{
			CustomerId: "c-out", IdempotencyKey: key, Items: []*orderingv1.CreateOrderItem{{Sku: "OUT-1", Quantity: 1}},
		}))
	}
	first, err := create("k-1")
	if err != nil {
		t.Fatalf("CreateOrder with the catalog up: %v", err)
	}
	orderCount := func() int {
		res, err := orders.ListOrders(c, connect.NewRequest(&orderingv1.ListOrdersRequest{CustomerId: "c-out"}))
		if err != nil {
			t.Fatalf("ListOrders: %v", err)
		}

		return len(res.Msg.GetOrders())
	}

	for round := 1; round <= 2; round++ { // down, up, down, up: a catalog that flaps recovers each time
		t.Run(fmt.Sprintf("outage %d", round), func(t *testing.T) {
			cat.Stop()

			_, err := create(fmt.Sprintf("k-down-%d", round))
			wantCode(t, err, connect.CodeUnavailable, "catalog.unavailable")
			if n := orderCount(); n != round {
				t.Fatalf("orders for c-out = %d after a refused CreateOrder, want %d (nothing written)", n, round)
			}
			// a replay of an order that exists is answered without asking the catalog
			again, err := create("k-1")
			if err != nil || again.Msg.GetOrder().GetId() != first.Msg.GetOrder().GetId() {
				t.Fatalf("replay with the catalog down = %v, %v; want the original order", again, err)
			}

			_, err = inv.SetStock(c, connect.NewRequest(&inventoryv1.SetStockRequest{Sku: "OUT-1", OnHand: 99}))
			wantCode(t, err, connect.CodeUnavailable, "catalog.unavailable")
			got, err := inv.GetStock(c, connect.NewRequest(&inventoryv1.GetStockRequest{Sku: "OUT-1"}))
			if err != nil || got.Msg.GetStock().GetOnHand() != 10 {
				t.Fatalf("GetStock after a refused SetStock = %v, %v; want on_hand 10 (nothing written)", got, err)
			}

			// everything that does not need the catalog is untouched, readyz included
			if _, err := orders.GetOrder(c, connect.NewRequest(&orderingv1.GetOrderRequest{Id: first.Msg.GetOrder().GetId()})); err != nil {
				t.Fatalf("GetOrder with the catalog down: %v", err)
			}
			res, err := http.Get(ts.URL + server.ReadyzPath)
			if err != nil || res.StatusCode != http.StatusOK {
				t.Fatalf("readyz with the catalog down = %v, %v; rpc must not depend on it", res, err)
			}
			_ = res.Body.Close()

			cat.Restart()
			// no rpc restart, no waiting: the next call works
			if _, err := create(fmt.Sprintf("k-up-%d", round)); err != nil {
				t.Fatalf("CreateOrder after the catalog came back: %v", err)
			}
			if _, err := inv.SetStock(c, connect.NewRequest(&inventoryv1.SetStockRequest{Sku: "OUT-1", OnHand: 10})); err != nil {
				t.Fatalf("SetStock after the catalog came back: %v", err)
			}
		})
	}
}

// A catalog that accepts the connection and never answers is UNAVAILABLE within the budget, not a hang.
func TestCatalogHang_IsUnavailableWithinTheBudget(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()

	ts := rpcServer(t, &config.AppConfig{Catalog: config.Catalog{Upstream: "http://" + ln.Addr().String(), APIKey: serviceKey, Timeout: 300 * time.Millisecond}})
	orders := orderingv1connect.NewOrderServiceClient(http.DefaultClient, ts.URL)
	start := time.Now()
	_, err = orders.CreateOrder(context.Background(), connect.NewRequest(&orderingv1.CreateOrderRequest{
		CustomerId: "c", Items: []*orderingv1.CreateOrderItem{{Sku: "ANY", Quantity: 1}},
	}))
	wantCode(t, err, connect.CodeUnavailable, "catalog.unavailable")
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("CreateOrder took %s against a hung catalog, the budget is 300ms", d)
	}
}

// A service key the catalog does not know is the deployment's mistake: users see an outage, never a
// credential problem.
func TestCatalogServiceKeyMismatch_LooksLikeAnOutageToUsers(t *testing.T) {
	cat := startCatalogProc(t)
	ts := rpcServer(t, &config.AppConfig{Catalog: config.Catalog{Upstream: cat.URL, APIKey: "some-other-key-0123456789abcd", Timeout: time.Second}})
	cl := catalogv1connect.NewCatalogServiceClient(http.DefaultClient, ts.URL)
	_, err := cl.GetProduct(context.Background(), connect.NewRequest(&catalogv1.GetProductRequest{Sku: "A"}))
	wantCode(t, err, connect.CodeUnavailable, "catalog.unavailable")
	if strings.Contains(err.Error(), "credential") || strings.Contains(err.Error(), "auth.") {
		t.Fatalf("the credential problem reaches the user: %v", err)
	}
}

// catalog.v1 on rpc keeps its rules: the role is checked on the USER's principal at rpc (the upstream
// only ever sees rpc's admin service key), and the catalog's own business errors arrive as they are.
func TestForwarding_RoleAtRPCAndBusinessErrorsUnchanged(t *testing.T) {
	cat := startCatalogProc(t)
	ts := rpcServer(t, withCatalog(&config.AppConfig{Auth: config.Auth{
		Mode: config.AuthRequired, APIKeys: "plain=plain-key-0123456789abc,ops=ops-key-0123456789abcd:admin",
	}}, cat))
	c := context.Background()
	as := func(key string) catalogv1connect.CatalogServiceClient {
		return catalogClientWithKey(ts.URL, key)
	}
	upsert := func(sku, ccy string) *connect.Request[catalogv1.UpsertProductRequest] {
		return connect.NewRequest(&catalogv1.UpsertProductRequest{Sku: sku, Name: "n", Price: &commonv1.Money{AmountMinor: 1, Currency: ccy}})
	}

	// non-admin writes are refused at rpc, and nothing reaches the catalog
	_, err := as("plain-key-0123456789abc").UpsertProduct(c, upsert("FWD-DENIED", "USD"))
	wantCode(t, err, connect.CodePermissionDenied, "auth.permission_denied")
	_, err = as("").UpsertProduct(c, upsert("FWD-ANON", "USD"))
	wantCode(t, err, connect.CodeUnauthenticated, "auth.unauthenticated")
	if _, err := as("ops-key-0123456789abcd").UpsertProduct(c, upsert("FWD-OK", "USD")); err != nil {
		t.Fatalf("admin write: %v", err)
	}
	if _, err := as("ops-key-0123456789abcd").GetProduct(c, connect.NewRequest(&catalogv1.GetProductRequest{Sku: "FWD-DENIED"})); err == nil {
		t.Fatal("a refused UpsertProduct reached the catalog")
	}
	// reads stay open to a role-less credential
	if _, err := as("plain-key-0123456789abc").GetProduct(c, connect.NewRequest(&catalogv1.GetProductRequest{Sku: "FWD-OK"})); err != nil {
		t.Fatalf("role-less read: %v", err)
	}

	// the catalog's business errors, code and x-error-code untouched
	_, err = as("ops-key-0123456789abcd").UpsertProduct(c, upsert("FWD-OK", "ZZZ"))
	wantCode(t, err, connect.CodeInvalidArgument, "product.unsupported_currency")
	_, err = as("ops-key-0123456789abcd").GetProduct(c, connect.NewRequest(&catalogv1.GetProductRequest{Sku: "NOPE"}))
	wantCode(t, err, connect.CodeNotFound, "product.not_found")
}

// catalog-rpc itself accepts the service key and nothing else, so the rpc's users cannot go around rpc.
func TestCatalogRPC_AcceptsOnlyTheServiceKey(t *testing.T) {
	cat := startCatalogProc(t)
	c := context.Background()
	// A request per call: the client's key interceptor sets X-API-Key ON the request, so a request shared
	// across the map's (random) order would carry the previous case's key into "no credential".
	get := func() *connect.Request[catalogv1.GetProductRequest] {
		return connect.NewRequest(&catalogv1.GetProductRequest{Sku: "A"})
	}
	for _, tc := range []struct{ key, code string }{
		{"", "auth.unauthenticated"},
		{"ops-key-0123456789abcd", "auth.invalid_credential"},
		{serviceKey + "x", "auth.invalid_credential"},
	} {
		_, err := catalogClientWithKey(cat.URL, tc.key).GetProduct(c, get())
		wantCode(t, err, connect.CodeUnauthenticated, tc.code)
	}
	_, err := catalogClientWithKey(cat.URL, serviceKey).GetProduct(c, get())
	wantCode(t, err, connect.CodeNotFound, "product.not_found")
}

// The trace of the request that asked for an order is the trace the catalog serves QuotePrices under,
// and the catalog logs its own access line for it.
func TestTrace_QuotePricesIsServedUnderTheCallersTrace(t *testing.T) {
	cat := startCatalogProc(t)
	ts := rpcServer(t, withCatalog(&config.AppConfig{}, cat))
	seedProduct(t, ts.URL, "", "TR-1", 100, "USD")

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	req := connect.NewRequest(&orderingv1.CreateOrderRequest{CustomerId: "c-tr", Items: []*orderingv1.CreateOrderItem{{Sku: "TR-1", Quantity: 1}}})
	req.Header().Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	if _, err := orderingv1connect.NewOrderServiceClient(http.DefaultClient, ts.URL).CreateOrder(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	var line map[string]any
	for _, l := range strings.Split(cat.logs.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["msg"] == "rpc" && strings.HasSuffix(fmt.Sprint(m["procedure"]), "/QuotePrices") {
			line = m
		}
	}
	if line == nil {
		t.Fatalf("catalog-rpc logged no access line for QuotePrices:\n%s", cat.logs.String())
	}
	if line["trace_id"] != traceID {
		t.Fatalf("QuotePrices ran under trace %v, want the caller's %s", line["trace_id"], traceID)
	}
}

// Children are reaped: a catalog-rpc whose deadline is shorter than the test's is killed and waited for,
// not left behind.
func TestCatalogProc_DeadlineKillsAndReaps(t *testing.T) {
	p := startCatalogProcWith(t, 4*time.Second)
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the child outlived its deadline")
	}
	if p.cmd.ProcessState == nil {
		t.Fatal("the child was killed but never waited for")
	}
	if res, err := http.Get(p.URL + server.ReadyzPath); err == nil {
		_ = res.Body.Close()
		t.Fatal("the child still answers after its deadline")
	}
}

func catalogClientWithKey(baseURL, key string) catalogv1connect.CatalogServiceClient {
	var opts []connect.ClientOption
	if key != "" {
		opts = append(opts, connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(c context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				req.Header().Set(server.APIKeyHeader, key)

				return next(c, req)
			}
		})))
	}

	return catalogv1connect.NewCatalogServiceClient(http.DefaultClient, baseURL, opts...)
}
