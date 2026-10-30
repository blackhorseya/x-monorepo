package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"connectrpc.com/connect"

	catalogv1 "github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1/catalogv1connect"
	catalogpg "github.com/blackhorseya/x-monorepo/pkgs/catalog/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/buildinfo"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	commonv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/common/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// freshDB migrates the catalog schema on the test Postgres (apps/migrate owns
// the schema in production; this binary never migrates) and empties it.
func freshDB(t *testing.T) config.Postgres {
	t.Helper()
	cfg := config.Postgres{URL: pgtest.URL(t)}
	c := context.Background()
	if err := postgres.Up(c, cfg, catalogpg.Migration()); err != nil {
		t.Fatalf("migrate catalog: %v", err)
	}
	tel, closeTel, err := otel.New(config.OTel{}, config.Log{Level: "error", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTel()
	db, closeDB, err := postgres.NewDB(cfg, tel)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	if err := db.WithContext(c).Exec("TRUNCATE catalog.products").Error; err != nil {
		t.Fatal(err)
	}

	return cfg
}

type catalogEnv struct {
	url string
	// client talks to the service with the given X-API-Key ("" = none).
	client func(key string) catalogv1connect.CatalogServiceClient
}

func startCatalog(t *testing.T, auth config.Auth) catalogEnv {
	t.Helper()
	srv, cleanup, err := Initialize(&config.AppConfig{
		HTTP:     config.HTTP{Addr: ":0"},
		Log:      config.Log{Level: "error", Format: "text"},
		Auth:     auth,
		Postgres: freshDB(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return catalogEnv{url: ts.URL, client: func(key string) catalogv1connect.CatalogServiceClient {
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
	}}
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

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)

	return res.StatusCode, string(b)
}

// CRUD and QuotePrices through the real server on a real Postgres: a reprice
// is what Get returns next, a deactivated product stays listed but is not
// quotable.
func TestCatalog_RoundTrip(t *testing.T) {
	c := context.Background()
	cl := startCatalog(t, config.Auth{Mode: config.AuthOff}).client("")

	_, err := cl.GetProduct(c, connect.NewRequest(&catalogv1.GetProductRequest{Sku: "A"}))
	wantConnectCode(t, err, connect.CodeNotFound, "product.not_found")
	for sku, price := range map[string]int64{"A": 150, "B": 90, "OLD": 5} {
		if _, err := cl.UpsertProduct(c, upsertReq(sku, price, "USD")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cl.UpsertProduct(c, upsertReq("A", 200, "USD")); err != nil {
		t.Fatal(err)
	}
	got, err := cl.GetProduct(c, connect.NewRequest(&catalogv1.GetProductRequest{Sku: "A"}))
	if err != nil || got.Msg.GetProduct().GetPrice().GetAmountMinor() != 200 {
		t.Fatalf("Get after reprice = %v, %v", got, err)
	}

	_, err = cl.UpsertProduct(c, upsertReq("B", -1, "USD"))
	wantConnectCode(t, err, connect.CodeInvalidArgument, "request.validation_failed")
	_, err = cl.UpsertProduct(c, upsertReq("B", 1, "ZZZ"))
	wantConnectCode(t, err, connect.CodeInvalidArgument, "product.unsupported_currency")

	if _, err := cl.DeactivateProduct(c, connect.NewRequest(&catalogv1.DeactivateProductRequest{Sku: "OLD"})); err != nil {
		t.Fatal(err)
	}
	_, err = cl.DeactivateProduct(c, connect.NewRequest(&catalogv1.DeactivateProductRequest{Sku: "NOPE"}))
	wantConnectCode(t, err, connect.CodeNotFound, "product.not_found")
	list, err := cl.ListProducts(c, connect.NewRequest(&catalogv1.ListProductsRequest{}))
	if err != nil || len(list.Msg.GetProducts()) != 3 {
		t.Fatalf("List = %v, %v; want 3 products (the inactive one too)", list, err)
	}

	res, err := cl.QuotePrices(c, connect.NewRequest(&catalogv1.QuotePricesRequest{Skus: []string{"B", "NOPE", "A", "OLD", "A", "GONE"}}))
	if err != nil {
		t.Fatal(err)
	}
	m := res.Msg
	if len(m.GetPrices()) != 2 || m.GetPrices()["A"].GetAmountMinor() != 200 || m.GetPrices()["B"].GetCurrency() != "USD" {
		t.Fatalf("prices = %v", m.GetPrices())
	}
	if got := m.GetMissing(); len(got) != 2 || got[0] != "NOPE" || got[1] != "GONE" {
		t.Fatalf("missing = %v, want [NOPE GONE]", got)
	}
	if got := m.GetInactive(); len(got) != 1 || got[0] != "OLD" {
		t.Fatalf("inactive = %v, want [OLD]", got)
	}
	_, err = cl.QuotePrices(c, connect.NewRequest(&catalogv1.QuotePricesRequest{}))
	wantConnectCode(t, err, connect.CodeInvalidArgument, "request.validation_failed")
}

// The data is in Postgres, not in the process: a second server on the same
// database sees what the first one wrote.
func TestCatalog_PersistsInPostgres(t *testing.T) {
	c := context.Background()
	first := startCatalog(t, config.Auth{Mode: config.AuthOff})
	if _, err := first.client("").UpsertProduct(c, upsertReq("P", 321, "USD")); err != nil {
		t.Fatal(err)
	}
	srv, cleanup, err := Initialize(&config.AppConfig{
		HTTP:     config.HTTP{Addr: ":0"},
		Log:      config.Log{Level: "error", Format: "text"},
		Postgres: config.Postgres{URL: pgtest.URL(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	got, err := catalogv1connect.NewCatalogServiceClient(http.DefaultClient, ts.URL).GetProduct(c, connect.NewRequest(&catalogv1.GetProductRequest{Sku: "P"}))
	if err != nil || got.Msg.GetProduct().GetPrice().GetAmountMinor() != 321 {
		t.Fatalf("second server Get = %v, %v", got, err)
	}
}

// auth.mode required, static keys only: this binary has no identity BC and no
// CredentialResolver, so a key the config does not list is unauthenticated
// (fail closed), never looked up anywhere else.
func TestCatalog_AuthMatrix(t *testing.T) {
	c := context.Background()
	env := startCatalog(t, config.Auth{
		Mode:    config.AuthRequired,
		APIKeys: "plain=plain-key-0123456789ab,ops=ops-key-0123456789abc:admin",
	})
	plain, ops, anon, stranger := env.client("plain-key-0123456789ab"), env.client("ops-key-0123456789abc"), env.client(""), env.client("not-in-the-config-0123456789")

	// no credential, unknown key → 401
	_, err := anon.UpsertProduct(c, upsertReq("X", 1, "USD"))
	wantConnectCode(t, err, connect.CodeUnauthenticated, "auth.unauthenticated")
	_, err = anon.GetProduct(c, connect.NewRequest(&catalogv1.GetProductRequest{Sku: "X"}))
	wantConnectCode(t, err, connect.CodeUnauthenticated, "auth.unauthenticated")
	for name, call := range map[string]func() error{
		"read": func() error {
			_, err := stranger.ListProducts(c, connect.NewRequest(&catalogv1.ListProductsRequest{}))
			return err
		},
		"write": func() error { _, err := stranger.UpsertProduct(c, upsertReq("X", 1, "USD")); return err },
		"quote": func() error {
			_, err := stranger.QuotePrices(c, connect.NewRequest(&catalogv1.QuotePricesRequest{Skus: []string{"X"}}))
			return err
		},
	} {
		t.Run("unknown key "+name, func(t *testing.T) {
			wantConnectCode(t, call(), connect.CodeUnauthenticated, "auth.invalid_credential")
		})
	}

	// a role-less key may read but not write → 403
	_, err = plain.UpsertProduct(c, upsertReq("X", 1, "USD"))
	wantConnectCode(t, err, connect.CodePermissionDenied, "auth.permission_denied")
	_, err = plain.DeactivateProduct(c, connect.NewRequest(&catalogv1.DeactivateProductRequest{Sku: "X"}))
	wantConnectCode(t, err, connect.CodePermissionDenied, "auth.permission_denied")

	// the admin key writes
	if _, err := ops.UpsertProduct(c, upsertReq("X", 150, "USD")); err != nil {
		t.Fatalf("admin UpsertProduct: %v", err)
	}
	// reads stay open to any verified credential, QuotePrices included
	if res, err := plain.GetProduct(c, connect.NewRequest(&catalogv1.GetProductRequest{Sku: "X"})); err != nil || res.Msg.GetProduct().GetPrice().GetAmountMinor() != 150 {
		t.Fatalf("role-less read = %v, %v", res, err)
	}
	if res, err := plain.QuotePrices(c, connect.NewRequest(&catalogv1.QuotePricesRequest{Skus: []string{"X"}})); err != nil || res.Msg.GetPrices()["X"].GetAmountMinor() != 150 {
		t.Fatalf("role-less quote = %v, %v", res, err)
	}
	// the refused writes changed nothing
	if _, err := plain.GetProduct(c, connect.NewRequest(&catalogv1.GetProductRequest{Sku: "NOPE"})); err == nil {
		t.Fatal("unknown product found")
	}
}

// Only auth.api_keys can authenticate: with auth on and no static key
// configured the server refuses to build instead of running open or asking a
// resolver that is not there.
func TestCatalog_AuthWithoutKeysFailsToStart(t *testing.T) {
	_, cleanup, err := Initialize(&config.AppConfig{
		HTTP:     config.HTTP{Addr: ":0"},
		Log:      config.Log{Level: "error", Format: "text"},
		Auth:     config.Auth{Mode: config.AuthRequired},
		Postgres: freshDB(t),
	})
	if err == nil {
		cleanup()
		t.Fatal("auth.mode=required without api_keys started")
	}
}

// Probes, the stamped version and the catalog-only surface.
func TestCatalog_Probes(t *testing.T) {
	env := startCatalog(t, config.Auth{Mode: config.AuthOff})

	if code, body := get(t, env.url+server.HealthzPath); code != http.StatusOK {
		t.Fatalf("healthz = %d %s", code, body)
	}
	code, body := get(t, env.url+server.ReadyzPath)
	if code != http.StatusOK || !strings.Contains(body, "postgres") {
		t.Fatalf("readyz = %d %q, want 200 naming the postgres check", code, body)
	}
	code, body = get(t, env.url+server.VersionPath)
	var info buildinfo.Info
	if code != http.StatusOK || json.Unmarshal([]byte(body), &info) != nil || info.Version != "dev" || info.Go != runtime.Version() {
		t.Fatalf("version = %d %q", code, body)
	}

	// no other BC is mounted: ordering's procedure path is not served
	res, err := http.Post(env.url+"/ordering.v1.OrderService/GetOrder", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("ordering procedure = %d, want 404 (catalog is the only BC here)", res.StatusCode)
	}
}

// readyz follows Postgres: once the database stops answering, the pod is not ready.
func TestCatalog_ReadyzFollowsPostgres(t *testing.T) {
	cfg := freshDB(t)
	srv, cleanup, err := Initialize(&config.AppConfig{
		HTTP:     config.HTTP{Addr: ":0"},
		Log:      config.Log{Level: "error", Format: "text"},
		Postgres: cfg,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	if code, _ := get(t, ts.URL+server.ReadyzPath); code != http.StatusOK {
		t.Fatalf("readyz with Postgres up = %d", code)
	}
	cleanup() // closes the pool: the probe now fails
	if code, _ := get(t, ts.URL+server.ReadyzPath); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz after the pool closed = %d, want 503", code)
	}
}
