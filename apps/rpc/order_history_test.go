package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	catalogpg "github.com/blackhorseya/x-monorepo/pkgs/catalog/infra/postgres"
	inventorypg "github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1/orderingv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	orderingpg "github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// GetOrderHistory through the whole wired apps/rpc on real Postgres, from the real auth entry:
// (auth.mode off | optional | required) x (admin | customer | another customer | an "admin" that is not
// auth.admin_role | anonymous) x (the order's owner's, someone else's, an order with no history, one that
// does not exist). The rows are written by real CreateOrder / CancelOrder calls made with real keys, so
// actor_subject is non-empty in the table (anchored below) and every hiding assertion is compared with
// what the admin sees for the same rows.
//
// auth.mode=off is the default: its cell is built from config.Load() with nothing set, and a request that
// carries an admin key there is still anonymous (the interceptor does not exist), so it must not see subjects.
func TestOrderHistory_AuthMatrix(t *testing.T) {
	c := context.Background()
	pgCfg := config.Postgres{URL: pgtest.URL(t)}
	db := pgtest.DB(t, pgCfg, pgtest.Telemetry(t))
	if err := postgres.Up(c, pgCfg, orderingpg.Migration()); err != nil {
		t.Fatal(err)
	}
	// Every other BC the wired server needs on the same database, as in production.
	for _, m := range []postgres.Migration{inventorypg.Migration(), catalogpg.Migration()} {
		if err := postgres.Up(c, pgCfg, m); err != nil {
			t.Fatal(err)
		}
	}

	cat := startCatalogProc(t)
	seedProduct(t, cat.URL, serviceKey, "SKU-1", 100, "USD")

	const (
		aliceKey = "alice-key-0123456789"
		bobKey   = "bob-key-0123456789"
		opsKey   = "ops-key-0123456789abc"
		decoyKey = "decoy-key-0123456789ab"
	)
	authCfg := func(mode string) config.Auth {
		return config.Auth{
			Mode:      mode,
			AdminRole: "root", // not the default: the role name must come from config
			APIKeys:   "alice=" + aliceKey + ",bob=" + bobKey + ",ops=" + opsKey + ":root,decoy=" + decoyKey + ":admin",
		}
	}
	start := func(cfg *config.AppConfig) string {
		srv, cleanup, err := initialize(t, withCatalog(cfg, cat))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cleanup)
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)

		return ts.URL
	}
	appCfg := func(auth config.Auth) *config.AppConfig {
		return &config.AppConfig{HTTP: config.HTTP{Addr: ":0"}, Log: config.Log{Level: "error", Format: "text"}, Auth: auth, Postgres: pgCfg}
	}

	// The default cell: config.Load() with nothing about auth set.
	t.Setenv("APP_CONFIG", "/dev/null")
	t.Setenv("APP_POSTGRES_URL", pgCfg.URL)
	t.Setenv("APP_HTTP_ADDR", ":0")
	t.Setenv("APP_LOG_LEVEL", "error")
	t.Setenv("APP_CATALOG_UPSTREAM", cat.URL)
	t.Setenv("APP_CATALOG_API_KEY", serviceKey)
	defaults, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Auth.Mode != config.AuthOff {
		t.Fatalf("the default auth.mode is %q, this matrix labels it %q", defaults.Auth.Mode, config.AuthOff)
	}
	urls := map[string]string{
		config.AuthOff:      start(defaults),
		config.AuthOptional: start(appCfg(authCfg(config.AuthOptional))),
		config.AuthRequired: start(appCfg(authCfg(config.AuthRequired))),
	}

	client := func(mode, key string) orderingv1connect.OrderServiceClient {
		var opts []connect.ClientOption
		if key != "" {
			opts = append(opts, connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
				return func(c context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
					req.Header().Set(server.APIKeyHeader, key)
					return next(c, req)
				}
			})))
		}

		return orderingv1connect.NewOrderServiceClient(http.DefaultClient, urls[mode], opts...)
	}

	// --- Fixtures, written through the auth-on server so the rows carry real subjects. ---
	var created []string
	t.Cleanup(func() { // while the pool is open: registered after pgtest.DB's close, so it runs first
		for _, id := range created {
			for _, q := range []string{`DELETE FROM ordering.order_history WHERE order_id = ?`, `DELETE FROM ordering.orders WHERE id = ?`} {
				if err := db.WithContext(c).Exec(q, id).Error; err != nil {
					t.Errorf("cleanup %s: %v", id, err)
				}
			}
		}
	})
	items := []*orderingv1.CreateOrderItem{{Sku: "SKU-1", Quantity: 1}}
	place := func(key, customer string) string {
		t.Helper()
		res, err := client(config.AuthOptional, key).CreateOrder(c, connect.NewRequest(&orderingv1.CreateOrderRequest{CustomerId: customer, Items: items}))
		if err != nil {
			t.Fatalf("CreateOrder as %s: %v", key, err)
		}
		id := res.Msg.GetOrder().GetId()
		created = append(created, id)

		return id
	}
	cancel := func(key, id string) {
		t.Helper()
		if _, err := client(config.AuthOptional, key).CancelOrder(c, connect.NewRequest(&orderingv1.CancelOrderRequest{Id: id})); err != nil {
			t.Fatalf("CancelOrder %s as %s: %v", id, key, err)
		}
	}
	aliceOrder := place(aliceKey, "")
	cancel(aliceKey, aliceOrder)
	bobOrder := place(bobKey, "")
	opsOrder := place(opsKey, "walk-in")
	cancel(opsKey, opsOrder)

	legacyOrder := "legacy-" + time.Now().UTC().Format("150405.000000")
	created = append(created, legacyOrder)
	usd, _ := kernel.NewMoney(100, "USD")
	li, err := order.NewLineItem("SKU-1", 1, usd)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := order.Reconstitute(order.ReconstituteParams{
		ID: order.ID(legacyOrder), CustomerID: "alice", Status: order.StatusConfirmed,
		CreatedAt: time.Now().UTC().Add(-time.Hour), UpdatedAt: time.Now().UTC().Add(-time.Hour), Items: []order.LineItem{li},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := orderingpg.NewOrderRepository(db).Save(c, legacy); err != nil {
		t.Fatal(err)
	}

	// Anchor: what is stored. If these were empty every "hidden" cell below would pass for nothing.
	count := func(q string, args ...any) int64 {
		t.Helper()
		var n int64
		if err := db.WithContext(c).Raw(q, args...).Scan(&n).Error; err != nil {
			t.Fatal(err)
		}

		return n
	}
	if n := count(`SELECT count(*) FROM ordering.order_history WHERE order_id = ? AND actor_subject = 'alice' AND actor_kind = 'CUSTOMER'`, aliceOrder); n != 2 {
		t.Fatalf("alice's order has %d CUSTOMER rows with subject alice in the table, want 2", n)
	}
	if n := count(`SELECT count(*) FROM ordering.order_history WHERE order_id = ? AND actor_subject = 'ops' AND actor_kind = 'ADMIN'`, opsOrder); n != 2 {
		t.Fatalf("the admin's order has %d ADMIN rows with subject ops in the table, want 2", n)
	}
	if n := count(`SELECT count(*) FROM ordering.order_history WHERE order_id = ?`, legacyOrder); n != 0 {
		t.Fatalf("the legacy order has %d history rows, want none", n)
	}

	// --- The matrix. ---
	type cell struct {
		mode, who, order string
		code             connect.Code // 0 = answered
		errCode          string       // x-error-code when it is an error
		kinds, subjects  []string
	}
	keys := map[string]string{"admin": opsKey, "customer": aliceKey, "other customer": bobKey, "not the admin role": decoyKey, "anonymous": ""}
	ids := map[string]string{"alice's": aliceOrder, "bob's": bobOrder, "the admin's": opsOrder, "no history": legacyOrder, "missing": "no-such-order"}
	cust, adm := []string{"CUSTOMER", "CUSTOMER"}, []string{"ADMIN", "ADMIN"}
	none := []string{"", ""}

	var cells []cell
	ok := func(mode, who, ord string, kinds, subjects []string) {
		cells = append(cells, cell{mode: mode, who: who, order: ord, kinds: kinds, subjects: subjects})
	}
	notFound := func(mode, who, ord string) {
		cells = append(cells, cell{mode: mode, who: who, order: ord, code: connect.CodeNotFound, errCode: "order.not_found"})
	}

	// auth.mode off (the default): every request is anonymous, whatever key it carries. Nothing is
	// confined, nobody sees a subject — not even the one holding the admin key.
	for _, who := range []string{"admin", "customer", "anonymous"} {
		ok(config.AuthOff, who, "alice's", cust, none)
		ok(config.AuthOff, who, "bob's", []string{"CUSTOMER"}, []string{""})
		ok(config.AuthOff, who, "the admin's", adm, none)
		ok(config.AuthOff, who, "no history", []string{}, []string{})
		notFound(config.AuthOff, who, "missing")
	}
	// optional and required behave the same for everyone who sent a credential.
	for _, mode := range []string{config.AuthOptional, config.AuthRequired} {
		ok(mode, "admin", "alice's", cust, []string{"alice", "alice"})
		ok(mode, "admin", "bob's", []string{"CUSTOMER"}, []string{"bob"})
		ok(mode, "admin", "the admin's", adm, []string{"ops", "ops"})
		ok(mode, "admin", "no history", []string{}, []string{})
		notFound(mode, "admin", "missing")

		ok(mode, "customer", "alice's", cust, none) // their own: kinds shown, subject hidden
		ok(mode, "customer", "no history", []string{}, []string{})
		for _, ord := range []string{"bob's", "the admin's", "missing"} {
			notFound(mode, "customer", ord)
		}
		ok(mode, "other customer", "bob's", []string{"CUSTOMER"}, []string{""})
		for _, ord := range []string{"alice's", "the admin's", "no history", "missing"} {
			notFound(mode, "other customer", ord)
		}
		// A role called "admin" is not auth.admin_role ("root"): this caller is a plain customer.
		for _, ord := range []string{"alice's", "bob's", "the admin's", "no history", "missing"} {
			notFound(mode, "not the admin role", ord)
		}
	}
	ok(config.AuthOptional, "anonymous", "alice's", cust, none)
	ok(config.AuthOptional, "anonymous", "bob's", []string{"CUSTOMER"}, []string{""})
	ok(config.AuthOptional, "anonymous", "the admin's", adm, none)
	ok(config.AuthOptional, "anonymous", "no history", []string{}, []string{})
	notFound(config.AuthOptional, "anonymous", "missing")
	for _, ord := range []string{"alice's", "missing"} {
		cells = append(cells, cell{mode: config.AuthRequired, who: "anonymous", order: ord, code: connect.CodeUnauthenticated, errCode: server.UnauthenticatedCode})
	}

	for _, cl := range cells {
		name := cl.mode + "/" + cl.who + "/" + cl.order
		t.Run(name, func(t *testing.T) {
			res, err := client(cl.mode, keys[cl.who]).GetOrderHistory(c, connect.NewRequest(&orderingv1.GetOrderHistoryRequest{Id: ids[cl.order]}))
			if cl.code != 0 {
				var ce *connect.Error
				if !errors.As(err, &ce) || ce.Code() != cl.code || ce.Meta().Get("x-error-code") != cl.errCode {
					t.Fatalf("err = %v (res %v), want %s / %s", err, res, cl.code, cl.errCode)
				}

				return
			}
			if err != nil {
				t.Fatalf("GetOrderHistory = %v, want an answer", err)
			}
			got := res.Msg.GetEntries()
			if len(got) != len(cl.kinds) {
				t.Fatalf("%d entries, want %d: %v", len(got), len(cl.kinds), got)
			}
			for i, e := range got {
				if e.GetSeq() != int32(i+1) { //nolint:gosec // i < 4
					t.Errorf("entry %d has seq %d, want %d", i, e.GetSeq(), i+1)
				}
				if kind := e.GetActorKind().String(); kind != "ORDER_HISTORY_ACTOR_KIND_"+cl.kinds[i] {
					t.Errorf("entry %d actor_kind = %s, want %s", i, kind, cl.kinds[i])
				}
				if e.GetActorSubject() != cl.subjects[i] {
					t.Errorf("entry %d actor_subject = %q, want %q", i, e.GetActorSubject(), cl.subjects[i])
				}
				if e.GetCause() != orderingv1.OrderHistoryCause_ORDER_HISTORY_CAUSE_RPC_REQUEST {
					t.Errorf("entry %d cause = %s, want RPC_REQUEST", i, e.GetCause())
				}
				if e.GetOccurredTime().AsTime().IsZero() {
					t.Errorf("entry %d has no occurred_time", i)
				}
			}
			if len(got) == 2 && (got[0].GetToStatus() != orderingv1.OrderStatus_ORDER_STATUS_PENDING || got[1].GetToStatus() != orderingv1.OrderStatus_ORDER_STATUS_CANCELLED ||
				got[0].GetFromStatus() != orderingv1.OrderStatus_ORDER_STATUS_UNSPECIFIED || got[1].GetFromStatus() != orderingv1.OrderStatus_ORDER_STATUS_PENDING) {
				t.Errorf("transitions = %v, want (none)→PENDING then PENDING→CANCELLED", got)
			}
		})
	}
}
