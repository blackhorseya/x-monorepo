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
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	paymentv1 "github.com/blackhorseya/x-monorepo/pkgs/payment/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/contract/v1/paymentv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	paymentpg "github.com/blackhorseya/x-monorepo/pkgs/payment/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/buildinfo"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

const (
	serviceKey = "payment-service-key-0123456789"
	owner      = "cust-owner"
	stranger   = "cust-stranger"
)

// freshDB migrates the payment schema on the test Postgres (apps/migrate owns the schema in
// production; this binary never migrates) and empties it, again on the way out BEFORE the pool of
// the migration helper is gone and with the error checked, so a dirty table never reaches the next
// test or the next -count round.
func freshDB(t *testing.T) config.Postgres {
	t.Helper()
	cfg := config.Postgres{URL: pgtest.URL(t)}
	c := context.Background()
	if err := postgres.Up(c, cfg, paymentpg.Migration()); err != nil {
		t.Fatalf("migrate payment: %v", err)
	}
	tel, closeTel, err := otel.New(config.OTel{}, config.Log{Level: "error", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeTel)
	db, closeDB, err := postgres.NewDB(cfg, tel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeDB)
	truncate := func() {
		if err := db.WithContext(c).Exec("TRUNCATE payment.payments").Error; err != nil {
			t.Errorf("truncate payment.payments: %v", err)
		}
	}
	truncate()
	t.Cleanup(truncate)

	return cfg
}

// seed stores a payment the way slice 3's consumer will (T1): PENDING, window open for 30 minutes.
func seed(t *testing.T, cfg config.Postgres, id, order, customer string, mutate func(*payment.Payment)) {
	t.Helper()
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
	total, err := kernel.NewMoney(1000, "TWD")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	p, err := payment.New(payment.NewParams{ID: payment.ID(id), OrderID: order, CustomerID: customer, Total: total, ExpiresAt: now.Add(30 * time.Minute)}, now)
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(p)
	}
	if err = paymentpg.NewPaymentRepository(db).Save(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

type paymentEnv struct {
	url string
	cfg config.Postgres
	// client talks to the service with the given X-API-Key ("" = none).
	client func(key string) paymentv1connect.PaymentServiceClient
}

// startPayment builds the REAL server (the same Initialize main uses, the real interceptor chain, a
// real Postgres) around provider. A nil provider is the mock.
func startPayment(t *testing.T, auth config.Auth, provider port.PaymentProvider) paymentEnv {
	t.Helper()
	cfg := freshDB(t)
	if provider == nil {
		provider = mock.New(mock.Options{})
	}
	srv, cleanup, err := Initialize(&config.AppConfig{
		HTTP:     config.HTTP{Addr: ":0"},
		Log:      config.Log{Level: "error", Format: "text"},
		Auth:     auth,
		Postgres: cfg,
	}, provider)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return paymentEnv{url: ts.URL, cfg: cfg, client: clientFor(ts.URL)}
}

func clientFor(url string) func(key string) paymentv1connect.PaymentServiceClient {
	return func(key string) paymentv1connect.PaymentServiceClient {
		var opts []connect.ClientOption
		if key != "" {
			opts = append(opts, connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
				return func(c context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
					req.Header().Set(server.APIKeyHeader, key)

					return next(c, req)
				}
			})))
		}

		return paymentv1connect.NewPaymentServiceClient(http.DefaultClient, url, opts...)
	}
}

// requiredAuth is what deployments run: auth required, the one service key.
var requiredAuth = config.Auth{Mode: config.AuthRequired, APIKeys: "rpc=" + serviceKey + ":admin"}

func wantConnectCode(t *testing.T, err error, code connect.Code, errorCode string) {
	t.Helper()
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != code || ce.Meta().Get("x-error-code") != errorCode {
		t.Fatalf("err = %v, want %s / %s", err, code, errorCode)
	}
}

func startReq(order, customer string, admin bool) *connect.Request[paymentv1.StartPaymentRequest] {
	return connect.NewRequest(&paymentv1.StartPaymentRequest{OrderId: order, CustomerId: customer, Admin: admin})
}

func getReq(order, customer string, admin bool) *connect.Request[paymentv1.GetPaymentRequest] {
	return connect.NewRequest(&paymentv1.GetPaymentRequest{OrderId: order, CustomerId: customer, Admin: admin})
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

// StartPayment and GetPayment through the real server on a real Postgres: the URL a second call gets is
// the first one's, and it is what GetPayment shows.
func TestPayment_RoundTrip(t *testing.T) {
	c := context.Background()
	env := startPayment(t, requiredAuth, nil)
	seed(t, env.cfg, "pay-1", "ord-1", owner, nil)
	cl := env.client(serviceKey)

	before, err := cl.GetPayment(c, getReq("ord-1", owner, false))
	if err != nil {
		t.Fatal(err)
	}
	p := before.Msg.GetPayment()
	if p.GetId() != "pay-1" || p.GetStatus() != paymentv1.PaymentStatus_PAYMENT_STATUS_PENDING || p.GetAmount().GetAmountMinor() != 1000 ||
		p.GetAmount().GetCurrency() != "TWD" || p.GetCheckoutUrl() != "" || p.GetCheckoutExpiresAt() != nil || p.GetExpiresAt() == nil {
		t.Fatalf("before start: %+v", p)
	}

	first, err := cl.StartPayment(c, startReq("ord-1", owner, false))
	if err != nil {
		t.Fatal(err)
	}
	url := first.Msg.GetPayment().GetCheckoutUrl()
	if !strings.HasPrefix(url, "https://") || first.Msg.GetPayment().GetCheckoutExpiresAt() == nil {
		t.Fatalf("started: %+v", first.Msg.GetPayment())
	}
	again, err := cl.StartPayment(c, startReq("ord-1", owner, false))
	if err != nil || again.Msg.GetPayment().GetCheckoutUrl() != url {
		t.Fatalf("second start = %v, %v; want the same URL %q", again, err, url)
	}
	after, err := cl.GetPayment(c, getReq("ord-1", owner, false))
	if err != nil || after.Msg.GetPayment().GetCheckoutUrl() != url {
		t.Fatalf("get after start = %v, %v; want URL %q", after, err, url)
	}
}

// The data is in Postgres, not in the process: a second server on the same database sees what the
// first one wrote.
func TestPayment_PersistsInPostgres(t *testing.T) {
	c := context.Background()
	first := startPayment(t, config.Auth{Mode: config.AuthOff}, nil)
	seed(t, first.cfg, "pay-1", "ord-1", owner, nil)
	started, err := first.client("").StartPayment(c, startReq("ord-1", owner, false))
	if err != nil {
		t.Fatal(err)
	}

	srv, cleanup, err := Initialize(&config.AppConfig{
		HTTP:     config.HTTP{Addr: ":0"},
		Log:      config.Log{Level: "error", Format: "text"},
		Postgres: config.Postgres{URL: pgtest.URL(t)},
	}, mock.New(mock.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	got, err := clientFor(ts.URL)("").GetPayment(c, getReq("ord-1", owner, false))
	if err != nil || got.Msg.GetPayment().GetCheckoutUrl() != started.Msg.GetPayment().GetCheckoutUrl() {
		t.Fatalf("second server Get = %v, %v", got, err)
	}
}

// auth.mode required, static keys only: this binary has no identity BC and no CredentialResolver, so a
// key the config does not list is unauthenticated (fail closed). A request that does not get past the
// interceptor changes nothing.
func TestPayment_AuthMatrix(t *testing.T) {
	c := context.Background()
	env := startPayment(t, config.Auth{Mode: config.AuthRequired, APIKeys: "rpc=" + serviceKey + ":admin"}, nil)
	seed(t, env.cfg, "pay-1", "ord-1", owner, nil)
	anon, stranger := env.client(""), env.client("not-in-the-config-0123456789")

	_, err := anon.StartPayment(c, startReq("ord-1", owner, false))
	wantConnectCode(t, err, connect.CodeUnauthenticated, "auth.unauthenticated")
	_, err = anon.GetPayment(c, getReq("ord-1", owner, false))
	wantConnectCode(t, err, connect.CodeUnauthenticated, "auth.unauthenticated")
	_, err = stranger.StartPayment(c, startReq("ord-1", owner, false))
	wantConnectCode(t, err, connect.CodeUnauthenticated, "auth.invalid_credential")
	_, err = stranger.GetPayment(c, getReq("ord-1", owner, true)) // claiming admin does not help
	wantConnectCode(t, err, connect.CodeUnauthenticated, "auth.invalid_credential")

	got, err := env.client(serviceKey).GetPayment(c, getReq("ord-1", owner, false))
	if err != nil || got.Msg.GetPayment().GetCheckoutUrl() != "" {
		t.Fatalf("after the refused calls: %v, %v; want the payment untouched (no checkout)", got, err)
	}
}

// Ownership over the wire (docs/payment.md §6): the claims of the request are believed because the
// caller holds the service key. A stranger sees NOT_FOUND for read and pay; an admin reads but cannot
// pay for someone else; a request that names nobody is INVALID_ARGUMENT, never "anyone".
func TestPayment_Ownership(t *testing.T) {
	c := context.Background()
	env := startPayment(t, requiredAuth, nil)
	seed(t, env.cfg, "pay-1", "ord-1", owner, nil)
	cl := env.client(serviceKey)

	if _, err := cl.GetPayment(c, getReq("ord-1", owner, false)); err != nil {
		t.Fatalf("owner read: %v", err)
	}
	_, err := cl.GetPayment(c, getReq("ord-1", stranger, false))
	wantConnectCode(t, err, connect.CodeNotFound, "payment.not_found")
	_, err = cl.GetPayment(c, getReq("ord-nope", stranger, false))
	wantConnectCode(t, err, connect.CodeNotFound, "payment.not_found") // same answer as for an order without a payment
	if res, err := cl.GetPayment(c, getReq("ord-1", stranger, true)); err != nil || res.Msg.GetPayment().GetCustomerId() != owner {
		t.Fatalf("admin read = %v, %v", res, err)
	}

	_, err = cl.StartPayment(c, startReq("ord-1", stranger, false))
	wantConnectCode(t, err, connect.CodeNotFound, "payment.not_found")
	_, err = cl.StartPayment(c, startReq("ord-1", stranger, true))
	wantConnectCode(t, err, connect.CodePermissionDenied, "payment.permission_denied")
	// none of the refused calls opened a checkout
	if res, _ := cl.GetPayment(c, getReq("ord-1", owner, false)); res.Msg.GetPayment().GetCheckoutUrl() != "" {
		t.Fatal("a refused StartPayment opened a checkout")
	}

	for name, call := range map[string]func() error{
		"start with no customer": func() error { _, err := cl.StartPayment(c, startReq("ord-1", "", false)); return err },
		"start admin, no customer": func() error {
			_, err := cl.StartPayment(c, startReq("ord-1", "", true))
			return err
		},
		"get with no customer": func() error { _, err := cl.GetPayment(c, getReq("ord-1", "", false)); return err },
		"get admin, no customer": func() error {
			_, err := cl.GetPayment(c, getReq("ord-1", "", true))
			return err
		},
		"start with no order": func() error { _, err := cl.StartPayment(c, startReq("", owner, false)); return err },
		"get with no order":   func() error { _, err := cl.GetPayment(c, getReq("", owner, false)); return err },
	} {
		t.Run(name, func(t *testing.T) {
			wantConnectCode(t, call(), connect.CodeInvalidArgument, "request.validation_failed")
		})
	}
	if _, err := cl.StartPayment(c, startReq("ord-1", owner, false)); err != nil {
		t.Fatalf("owner start after all the refusals: %v", err)
	}
}

// The state-dependent refusals reach the wire with their codes (the table itself is the domain's).
func TestPayment_StartRefusals(t *testing.T) {
	c := context.Background()
	env := startPayment(t, requiredAuth, nil)
	cl := env.client(serviceKey)
	seed(t, env.cfg, "pay-cap", "ord-cap", owner, func(p *payment.Payment) { p.Capture(payment.SourceWebhook, p.Amount(), time.Now()) })
	seed(t, env.cfg, "pay-canc", "ord-canc", owner, func(p *payment.Payment) { p.OrderCancelled(time.Now()) })

	_, err := cl.StartPayment(c, startReq("ord-cap", owner, false))
	wantConnectCode(t, err, connect.CodeFailedPrecondition, "payment.not_pending")
	_, err = cl.StartPayment(c, startReq("ord-canc", owner, false))
	wantConnectCode(t, err, connect.CodeFailedPrecondition, "payment.not_pending")
	_, err = cl.StartPayment(c, startReq("ord-none", owner, false))
	wantConnectCode(t, err, connect.CodeNotFound, "payment.not_found")

	// a window that closed while nobody swept it: payment.expired (the stored PENDING row is past its time)
	tel, closeTel, _ := otel.New(config.OTel{}, config.Log{Level: "error", Format: "text"})
	defer closeTel()
	db, closeDB, err := postgres.NewDB(env.cfg, tel)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	seed(t, env.cfg, "pay-old", "ord-old", owner, nil)
	if err = db.Exec(`UPDATE payment.payments SET expires_at = now() - interval '1 minute' WHERE id = 'pay-old'`).Error; err != nil {
		t.Fatal(err)
	}
	_, err = cl.StartPayment(c, startReq("ord-old", owner, false))
	wantConnectCode(t, err, connect.CodeFailedPrecondition, "payment.expired")
}

// A provider that does not answer: UNAVAILABLE with a fixed message, nothing changed, the same call works
// once the provider is back.
func TestPayment_ProviderUnavailable(t *testing.T) {
	c := context.Background()
	var mu sync.Mutex
	down := true
	prov := mock.New(mock.Options{Fault: func(string) error {
		mu.Lock()
		defer mu.Unlock()
		if down {
			return errors.New("down")
		}

		return nil
	}})
	env := startPayment(t, requiredAuth, prov)
	seed(t, env.cfg, "pay-1", "ord-1", owner, nil)
	cl := env.client(serviceKey)

	_, err := cl.StartPayment(c, startReq("ord-1", owner, false))
	wantConnectCode(t, err, connect.CodeUnavailable, "payment.provider_unavailable")
	if got, _ := cl.GetPayment(c, getReq("ord-1", owner, false)); got.Msg.GetPayment().GetCheckoutUrl() != "" {
		t.Fatal("a failed start left a checkout behind")
	}
	// GetPayment does not need the provider
	if _, err = cl.GetPayment(c, getReq("ord-1", owner, false)); err != nil {
		t.Fatalf("GetPayment with the provider down: %v", err)
	}

	mu.Lock()
	down = false
	mu.Unlock()
	if res, err := cl.StartPayment(c, startReq("ord-1", owner, false)); err != nil || res.Msg.GetPayment().GetCheckoutUrl() == "" {
		t.Fatalf("after recovery: %v, %v", res, err)
	}
}

// Only auth.api_keys can authenticate: with auth on and no static key configured the server refuses to
// build instead of running open or asking a resolver that is not there.
func TestPayment_AuthWithoutKeysFailsToStart(t *testing.T) {
	_, cleanup, err := Initialize(&config.AppConfig{
		HTTP:     config.HTTP{Addr: ":0"},
		Log:      config.Log{Level: "error", Format: "text"},
		Auth:     config.Auth{Mode: config.AuthRequired},
		Postgres: freshDB(t),
	}, mock.New(mock.Options{}))
	if err == nil {
		cleanup()
		t.Fatal("auth.mode=required without api_keys started")
	}
}

// Probes, the stamped version and the payment-only surface. readyz looks at Postgres and nothing else: a
// provider that is down fails StartPayment, it does not take the pod (and GetPayment with it) out of rotation.
func TestPayment_Probes(t *testing.T) {
	prov := mock.New(mock.Options{Fault: func(string) error { return errors.New("down") }})
	env := startPayment(t, config.Auth{Mode: config.AuthOff}, prov)

	if code, body := get(t, env.url+server.HealthzPath); code != http.StatusOK {
		t.Fatalf("healthz = %d %s", code, body)
	}
	code, body := get(t, env.url+server.ReadyzPath)
	if code != http.StatusOK || !strings.Contains(body, "postgres") || strings.Contains(strings.ToLower(body), "provider") {
		t.Fatalf("readyz = %d %q, want 200 naming the postgres check only (the provider is down here)", code, body)
	}
	code, body = get(t, env.url+server.VersionPath)
	var info buildinfo.Info
	if code != http.StatusOK || json.Unmarshal([]byte(body), &info) != nil || info.Version != "dev" || info.Go != runtime.Version() {
		t.Fatalf("version = %d %q", code, body)
	}

	// no other BC is mounted
	for _, path := range []string{"/ordering.v1.OrderService/GetOrder", "/catalog.v1.CatalogService/GetProduct"} {
		res, err := http.Post(env.url+path, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404 (payment is the only BC here)", path, res.StatusCode)
		}
	}
}

// readyz follows Postgres: once the database stops answering, the pod is not ready.
func TestPayment_ReadyzFollowsPostgres(t *testing.T) {
	cfg := freshDB(t)
	srv, cleanup, err := Initialize(&config.AppConfig{
		HTTP:     config.HTTP{Addr: ":0"},
		Log:      config.Log{Level: "error", Format: "text"},
		Postgres: cfg,
	}, mock.New(mock.Options{}))
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

// A legacy auth.jwt (secret or public key) is refused at build time, whatever the keys beside it: the
// only credentials here are auth.api_keys, and the admin / customer claims are believed only for them.
func TestPayment_LegacyJWTFailsToStart(t *testing.T) {
	for name, jwt := range map[string]config.JWT{
		"hs256 secret": {HS256Secret: "0123456789abcdef0123456789abcdef"},
		"public key":   {PublicKey: "-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n-----END PUBLIC KEY-----"},
	} {
		for _, keys := range []string{"", "ops=ops-key-0123456789abc:admin"} {
			_, cleanup, err := Initialize(&config.AppConfig{
				HTTP:     config.HTTP{Addr: ":0"},
				Log:      config.Log{Level: "error", Format: "text"},
				Auth:     config.Auth{Mode: config.AuthRequired, APIKeys: keys, JWT: jwt},
				Postgres: freshDB(t),
			}, mock.New(mock.Options{}))
			if err == nil {
				cleanup()
				t.Fatalf("%s with api_keys=%q started", name, keys)
			}
			if !strings.Contains(err.Error(), "auth.jwt") {
				t.Fatalf("%s with api_keys=%q: err = %v, want it to name auth.jwt", name, keys, err)
			}
		}
	}
}

// auth.mode off does not excuse an auth.jwt, and the message never carries the secret.
func TestPayment_LegacyJWTRefusedWithAuthOff(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	_, cleanup, err := Initialize(&config.AppConfig{
		HTTP:     config.HTTP{Addr: ":0"},
		Log:      config.Log{Level: "error", Format: "text"},
		Auth:     config.Auth{Mode: config.AuthOff, JWT: config.JWT{HS256Secret: secret}},
		Postgres: freshDB(t),
	}, mock.New(mock.Options{}))
	if err == nil {
		cleanup()
		t.Fatal("auth.jwt with auth.mode off started")
	}
	if !strings.Contains(err.Error(), "auth.jwt") || strings.Contains(err.Error(), secret) {
		t.Fatalf("err = %v, want it to name auth.jwt and not carry the secret", err)
	}
}

// auth.oidc.enabled is refused like auth.jwt, alone, beside a static key, and with auth.mode off.
func TestPayment_OIDCFailsToStart(t *testing.T) {
	for name, auth := range map[string]config.Auth{
		"oidc alone":         {Mode: config.AuthRequired},
		"oidc with api key":  {Mode: config.AuthRequired, APIKeys: "ops=ops-key-0123456789abc:admin"},
		"oidc with auth off": {Mode: config.AuthOff},
	} {
		t.Run(name, func(t *testing.T) {
			auth.OIDC.Enabled = true
			_, cleanup, err := Initialize(&config.AppConfig{
				HTTP:     config.HTTP{Addr: ":0"},
				Log:      config.Log{Level: "error", Format: "text"},
				Auth:     auth,
				Postgres: freshDB(t),
			}, mock.New(mock.Options{}))
			if err == nil {
				cleanup()
				t.Fatal("auth.oidc.enabled started")
			}
			if !strings.Contains(err.Error(), "auth.oidc") {
				t.Fatalf("err = %v, want it to name auth.oidc", err)
			}
		})
	}
}
