package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	catalogpg "github.com/blackhorseya/x-monorepo/pkgs/catalog/infra/postgres"
	inventorypg "github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	orderingpg "github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// The payment entry of apps/rpc (docs/payment.md §6, slice 4): payment.v1.PublicPaymentService, served
// by rpc and forwarded to payment-rpc with the service key and the caller's identity. These tests go
// through the whole wired rpc on Postgres with a fake payment-rpc behind it, over HTTP + Connect's JSON
// protocol so they depend on nothing but the wire: they fail on an rpc that does not serve the service
// at all, at the assertion about what it answers, not at a missing type.

const (
	payServiceKey = "payment-rpc-service-key-0123456789"
	aliceKey      = "alice-key-0123456789"
	bobKey        = "bob-key-0123456789ab"
	opsKey        = "ops-key-0123456789abc"
	leakMarker    = "LEAK-5f4dcc3b5aa765d61d8327deb882cf99"
	publicSvc     = "/payment.v1.PublicPaymentService/"
)

// fakePayment is payment-rpc as rpc sees it: the internal payment.v1.PaymentService over Connect JSON,
// behind the one service key. It records every call that got in.
type fakePayment struct {
	ts    *httptest.Server
	mu    sync.Mutex
	calls []payCall
	mode  string // ok | denied | leak | hang
}

type payCall struct {
	Method string
	Key    string
	Body   map[string]any
}

func newFakePayment(t *testing.T, mode string) *fakePayment {
	t.Helper()
	f := &fakePayment{mode: mode}
	f.ts = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.ts.Close)

	return f
}

func (f *fakePayment) setMode(mode string) { f.mu.Lock(); f.mode = mode; f.mu.Unlock() }

func (f *fakePayment) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

func (f *fakePayment) last(t *testing.T) payCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("payment-rpc was not called")
	}

	return f.calls[len(f.calls)-1]
}

func (f *fakePayment) serve(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	if r.Header.Get("X-API-Key") != payServiceKey {
		writeConnectError(w, http.StatusUnauthorized, "unauthenticated", "", "no service key")
		return
	}
	var body map[string]any
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.calls = append(f.calls, payCall{Method: method, Key: r.Header.Get("X-API-Key"), Body: body})
	mode := f.mode
	f.mu.Unlock()

	switch mode {
	case "hang":
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	case "denied":
		writeConnectError(w, http.StatusForbidden, "permission_denied", "payment.permission_denied", "an admin cannot pay for another customer")
	case "leak":
		// What a misbehaving upstream might send: its text, a header and a body full of something that
		// must never reach the caller or the log.
		w.Header().Set("X-Error-Code", leakMarker)
		w.Header().Set("X-Leak", leakMarker)
		writeConnectError(w, http.StatusInternalServerError, "internal", leakMarker, leakMarker)
	default:
		pay := map[string]any{
			"id": "pay-1", "orderId": body["orderId"], "customerId": body["customerId"],
			"status": "PAYMENT_STATUS_PENDING",
		}
		if method == "StartPayment" {
			pay["checkoutUrl"] = "https://pay.example.test/checkout/pay-1"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"payment": pay})
	}
}

func writeConnectError(w http.ResponseWriter, status int, code, xcode, msg string) {
	w.Header().Set("Content-Type", "application/json")
	if xcode != "" {
		w.Header().Set("x-error-code", xcode)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": msg})
}

type payOpts struct {
	upstream bool   // payment.upstream / payment.api_key set
	require  bool   // ordering.require_payment
	mode     string // the fake's mode
	env      map[string]string
}

type payHarness struct {
	url  string
	fake *fakePayment
	db   *gorm.DB
	logs *lockedBuffer
}

// stdoutTo points os.Stdout at a buffer until the test ends: the server's logger writes there, and it
// binds the file when it is built.
func stdoutTo(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = wr
	done := make(chan struct{})
	go func() { _, _ = io.Copy(buf, rd); close(done) }()
	t.Cleanup(func() {
		os.Stdout = orig
		_ = wr.Close()
		<-done
		_ = rd.Close()
	})

	return buf
}

func newPayHarness(t *testing.T, o payOpts) *payHarness {
	t.Helper()
	pgCfg := config.Postgres{URL: pgtest.URL(t)}
	c := context.Background()
	db := pgtest.DB(t, pgCfg, pgtest.Telemetry(t))
	for _, m := range []postgres.Migration{orderingpg.Migration(), inventorypg.Migration(), catalogpg.Migration()} {
		if err := postgres.Up(c, pgCfg, m); err != nil {
			t.Fatal(err)
		}
	}
	cat := startCatalogProc(t, "APP_POSTGRES_URL="+pgCfg.URL)
	fake := newFakePayment(t, o.mode)

	env := map[string]string{
		"APP_CONFIG": "/dev/null", "APP_HTTP_ADDR": ":0", "APP_LOG_LEVEL": "warn", "APP_LOG_FORMAT": "text",
		"APP_AUTH_MODE": "optional", "APP_AUTH_API_KEYS": "alice=" + aliceKey + ",bob=" + bobKey + ",ops=" + opsKey + ":admin",
		"APP_POSTGRES_URL": pgCfg.URL, "APP_CATALOG_UPSTREAM": cat.URL, "APP_CATALOG_API_KEY": serviceKey,
	}
	if o.upstream {
		env["APP_PAYMENT_UPSTREAM"] = fake.ts.URL
		env["APP_PAYMENT_API_KEY"] = payServiceKey
		env["APP_PAYMENT_TIMEOUT"] = "1s"
	}
	if o.require {
		env["APP_ORDERING_REQUIRE_PAYMENT"] = "true"
	}
	for k, v := range o.env {
		env[k] = v
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	logs := stdoutTo(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	srv, cleanup, err := initialize(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &payHarness{url: ts.URL, fake: fake, db: db, logs: logs}
}

// confirmed plants a CONFIRMED order the way the worker's ConfirmOrder stores it, with the payment
// snapshot given, for customer. The id is this call's own (the database outlives a test).
func (h *payHarness) confirmed(t *testing.T, customer string, required bool) string {
	t.Helper()
	c := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	usd, _ := kernel.NewMoney(250, "USD")
	li, err := order.NewLineItem("SKU-1", 1, usd)
	if err != nil {
		t.Fatal(err)
	}
	id := "pay-entry-" + uuid.NewString()
	o, err := order.New(order.Params{ID: order.ID(id), CustomerID: customer, Items: []order.LineItem{li}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Confirm(now, required); err != nil {
		t.Fatal(err)
	}
	if err := orderingpg.NewOrderRepository(h.db).Save(c, o); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.db.WithContext(c).Exec(`DELETE FROM ordering.orders WHERE id = ?`, id).Error; err != nil {
			t.Errorf("cleanup order %s: %v", id, err)
		}
	})

	return id
}

type payReply struct {
	status int
	code   string // Connect error code, "" on success
	xcode  string
	body   map[string]any
	raw    string
	header http.Header
}

// post calls one method of PublicPaymentService with a raw JSON body; key "" = no credential.
func (h *payHarness) post(t *testing.T, key, method, body string) payReply {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.url+publicSvc+method, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	r := payReply{status: res.StatusCode, xcode: res.Header.Get("x-error-code"), raw: string(raw), header: res.Header}
	_ = json.Unmarshal(raw, &r.body)
	if res.StatusCode != http.StatusOK {
		r.code, _ = r.body["code"].(string)
		if r.code == "" {
			r.code = "http_" + strconv.Itoa(res.StatusCode) // not a Connect error at all (a 404 page, say)
		}
	}

	return r
}

func wantReply(t *testing.T, r payReply, code, xcode string) {
	t.Helper()
	if r.code != code || r.xcode != xcode {
		t.Fatalf("reply = HTTP %d %s / %q (%s), want %s / %q", r.status, r.code, r.xcode, r.raw, code, xcode)
	}
}

func startBody(orderID string) string { return `{"orderId":"` + orderID + `"}` }

// Every cell of "ordering.require_payment (rpc's setting) × the order's payment_required snapshot" for
// StartPayment (docs/payment.md §8), the default cell included: nothing about payment configured at
// all. Only the cell where both are on reaches payment-rpc; in every other cell the answer is
// payment.disabled and payment-rpc is not asked.
func TestPaymentEntry_StartPayment_SwitchSnapshotMatrix(t *testing.T) {
	type cell struct {
		snapshot bool
		reaches  bool
	}
	for _, g := range []struct {
		name  string
		opts  payOpts
		cells []cell
	}{
		{"default config: nothing about payment set", payOpts{}, []cell{{false, false}, {true, false}}},
		{"payment-rpc configured, switch off", payOpts{upstream: true}, []cell{{false, false}, {true, false}}},
		{"payment-rpc configured, switch on", payOpts{upstream: true, require: true}, []cell{{false, false}, {true, true}}},
	} {
		t.Run(g.name, func(t *testing.T) {
			h := newPayHarness(t, g.opts)
			for _, c := range g.cells {
				id := h.confirmed(t, "alice", c.snapshot)
				before := h.fake.count()
				r := h.post(t, aliceKey, "StartPayment", startBody(id))
				if !c.reaches {
					wantReply(t, r, "failed_precondition", "payment.disabled")
					if h.fake.count() != before {
						t.Fatalf("snapshot=%v: payment-rpc was asked although the answer is payment.disabled", c.snapshot)
					}
					continue
				}
				if r.code != "" {
					t.Fatalf("snapshot=%v: reply = %s / %q (%s), want success", c.snapshot, r.code, r.xcode, r.raw)
				}
				if h.fake.count() != before+1 {
					t.Fatalf("payment-rpc calls = %d, want %d", h.fake.count(), before+1)
				}
				pay, _ := r.body["payment"].(map[string]any)
				if pay["checkoutUrl"] != "https://pay.example.test/checkout/pay-1" {
					t.Fatalf("payment = %v, want payment-rpc's answer passed through", pay)
				}
			}
		})
	}
}

// Who the caller is comes from the verified credential and nowhere else: a client that writes
// customer_id or admin into the body cannot make payment-rpc believe it. The service key is the only
// credential that reaches payment-rpc.
func TestPaymentEntry_IdentityIsTheCredentials_NotTheBody(t *testing.T) {
	h := newPayHarness(t, payOpts{upstream: true, require: true})
	aliceOrder := h.confirmed(t, "alice", true)

	// alice forges: another customer and the admin flag.
	r := h.post(t, aliceKey, "StartPayment", `{"orderId":"`+aliceOrder+`","customerId":"bob","admin":true}`)
	if r.code != "" {
		t.Fatalf("StartPayment = %s / %q (%s)", r.code, r.xcode, r.raw)
	}
	got := h.fake.last(t)
	if got.Method != "StartPayment" || got.Body["customerId"] != "alice" || got.Body["admin"] == true || got.Body["orderId"] != aliceOrder || got.Key != payServiceKey {
		t.Fatalf("payment-rpc received %+v, want StartPayment for alice, not admin, with the service key", got)
	}

	// GetPayment: same, for the owner and for an admin.
	h.post(t, aliceKey, "GetPayment", `{"orderId":"`+aliceOrder+`","customerId":"bob","admin":true}`)
	if got := h.fake.last(t); got.Method != "GetPayment" || got.Body["customerId"] != "alice" || got.Body["admin"] == true {
		t.Fatalf("payment-rpc received %+v for alice's GetPayment", got)
	}
	h.post(t, opsKey, "GetPayment", startBody(aliceOrder))
	if got := h.fake.last(t); got.Body["customerId"] != "ops" || got.Body["admin"] != true {
		t.Fatalf("payment-rpc received %+v for the admin's GetPayment, want customer ops, admin", got)
	}

	// An admin can read alice's order, so the call is forwarded as it is; whether an admin may pay for
	// someone else is payment-rpc's answer, and it is passed through unchanged.
	h.fake.setMode("denied")
	before := h.fake.count()
	wantReply(t, h.post(t, opsKey, "StartPayment", startBody(aliceOrder)), "permission_denied", "payment.permission_denied")
	if got := h.fake.last(t); h.fake.count() != before+1 || got.Body["customerId"] != "ops" || got.Body["admin"] != true {
		t.Fatalf("payment-rpc received %+v for the admin's StartPayment, want one call, customer ops, admin", got)
	}
	h.fake.setMode("ok")

	// Somebody else's order, and one that is not there: NOT_FOUND from ordering, payment-rpc not asked.
	before = h.fake.count()
	wantReply(t, h.post(t, bobKey, "StartPayment", startBody(aliceOrder)), "not_found", "order.not_found")
	wantReply(t, h.post(t, aliceKey, "StartPayment", startBody("no-such-order")), "not_found", "order.not_found")
	if h.fake.count() != before {
		t.Fatal("payment-rpc was asked about an order the caller cannot see")
	}

	// Anonymous: there is nobody to pay, and payment-rpc would call an empty customer_id a bug.
	for _, method := range []string{"StartPayment", "GetPayment"} {
		if r := h.post(t, "", method, startBody(aliceOrder)); r.code != "unauthenticated" {
			t.Fatalf("anonymous %s = %s / %q (%s), want unauthenticated", method, r.code, r.xcode, r.raw)
		}
	}
	if h.fake.count() != before {
		t.Fatal("payment-rpc was asked on behalf of nobody")
	}
}

// GetPayment is not behind the switch (docs/payment.md §6): with payment-rpc configured it is
// forwarded whatever the switch says; with nothing configured it is UNAVAILABLE.
func TestPaymentEntry_GetPayment_NotBehindTheSwitch(t *testing.T) {
	h := newPayHarness(t, payOpts{upstream: true})
	id := h.confirmed(t, "alice", false)
	r := h.post(t, aliceKey, "GetPayment", startBody(id))
	if r.code != "" || h.fake.count() != 1 {
		t.Fatalf("GetPayment with the switch off = %s / %q (%s), payment-rpc calls %d; want it forwarded", r.code, r.xcode, r.raw, h.fake.count())
	}

	bare := newPayHarness(t, payOpts{})
	id = bare.confirmed(t, "alice", false)
	wantReply(t, bare.post(t, aliceKey, "GetPayment", startBody(id)), "unavailable", "payment.unavailable")
}

// payment-rpc being down, slow or wrong is one answer for the caller, UNAVAILABLE payment.unavailable
// with a fixed message, and it touches nothing else: what the upstream wrote (its text, a header, an
// error code of its own) is not echoed to the caller and not written to the log. Ordering keeps working.
func TestPaymentEntry_PaymentRPCTrouble_IsOneFixedUnavailable(t *testing.T) {
	h := newPayHarness(t, payOpts{upstream: true, require: true, mode: "leak"})
	id := h.confirmed(t, "alice", true)

	r := h.post(t, aliceKey, "StartPayment", startBody(id))
	wantReply(t, r, "unavailable", "payment.unavailable")
	for name, text := range map[string]string{"body": r.raw, "headers": headerText(r.header), "log": h.logs.String()} {
		if strings.Contains(text, leakMarker) {
			t.Fatalf("what payment-rpc wrote reached the %s: %s", name, text)
		}
	}
	if h.fake.count() != 1 {
		t.Fatalf("payment-rpc calls = %d, want the one that failed (positive control: the marker was sent)", h.fake.count())
	}
	if !strings.Contains(h.logs.String(), "payment") {
		t.Fatalf("the failure left no line about payment in the log (positive control for the log capture): %q", h.logs.String())
	}

	// Slow: the call budget (payment.timeout, 1s here) ends it.
	h.fake.setMode("hang")
	start := time.Now()
	wantReply(t, h.post(t, aliceKey, "GetPayment", startBody(id)), "unavailable", "payment.unavailable")
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the hang took %s, want about the 1s budget", d)
	}

	// Gone: nothing is listening.
	h.fake.ts.Close()
	wantReply(t, h.post(t, aliceKey, "StartPayment", startBody(id)), "unavailable", "payment.unavailable")

	// Ordering is not affected.
	req, _ := http.NewRequest(http.MethodPost, h.url+"/ordering.v1.OrderService/GetOrder", strings.NewReader(`{"id":"`+id+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", aliceKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("GetOrder during the outage = %v, %v", res, err)
	}
	_ = res.Body.Close()
}

func headerText(h http.Header) string {
	var b strings.Builder
	for k, v := range h {
		b.WriteString(k + ": " + strings.Join(v, ",") + "\n")
	}

	return b.String()
}

// The new service sits behind the same interceptors as OrderService, with no list of methods of its
// own to forget: credentials required when auth.mode says so, the rate limit, the validation of the body.
func TestPaymentEntry_SameInterceptorsAsOrderService(t *testing.T) {
	t.Run("auth required", func(t *testing.T) {
		h := newPayHarness(t, payOpts{upstream: true, require: true, env: map[string]string{"APP_AUTH_MODE": "required"}})
		id := h.confirmed(t, "alice", true)
		wantReply(t, h.post(t, "", "GetPayment", startBody(id)), "unauthenticated", "auth.unauthenticated")
		if r := h.post(t, aliceKey, "GetPayment", startBody(id)); r.code != "" {
			t.Fatalf("GetPayment with a valid key = %s / %q", r.code, r.xcode)
		}
	})
	t.Run("rate limit", func(t *testing.T) {
		h := newPayHarness(t, payOpts{upstream: true, require: true, env: map[string]string{"APP_RATE_LIMIT_RPS": "1", "APP_RATE_LIMIT_BURST": "1"}})
		id := h.confirmed(t, "alice", true)
		limited := false
		for range 5 {
			if r := h.post(t, aliceKey, "GetPayment", startBody(id)); r.code == "resource_exhausted" {
				limited = true
				if r.xcode != "request.rate_limited" {
					t.Fatalf("limited with x-error-code %q", r.xcode)
				}
			}
		}
		if !limited {
			t.Fatal("five calls in a burst of one at 1 rps were never rate limited")
		}
	})
	t.Run("validation", func(t *testing.T) {
		h := newPayHarness(t, payOpts{upstream: true, require: true})
		wantReply(t, h.post(t, aliceKey, "StartPayment", `{}`), "invalid_argument", "request.validation_failed")
		if h.fake.count() != 0 {
			t.Fatal("payment-rpc was asked with an invalid request")
		}
	})
}

// payment is optional for rpc until it is switched on: nothing set boots (every existing test relies on
// it), the switch on without payment-rpc, or payment-rpc half configured, does not. The refusal never
// prints the key.
func TestPaymentEntry_StartupMatrix(t *testing.T) {
	const key = "a-payment-service-key-0123456789abcdef"
	for _, tc := range []struct {
		name  string
		env   map[string]string
		start bool
	}{
		{"nothing set (the default)", nil, true},
		{"switch on, nothing else", map[string]string{"APP_ORDERING_REQUIRE_PAYMENT": "true"}, false},
		{"switch on, upstream without key", map[string]string{"APP_ORDERING_REQUIRE_PAYMENT": "true", "APP_PAYMENT_UPSTREAM": "http://payment-rpc:80"}, false},
		{"switch on, key without upstream", map[string]string{"APP_ORDERING_REQUIRE_PAYMENT": "true", "APP_PAYMENT_API_KEY": key}, false},
		{"switch off, upstream without key", map[string]string{"APP_PAYMENT_UPSTREAM": "http://payment-rpc:80"}, false},
		{"switch off, key without upstream", map[string]string{"APP_PAYMENT_API_KEY": key}, false},
		{"switch off, upstream not a URL", map[string]string{"APP_PAYMENT_UPSTREAM": "payment-rpc", "APP_PAYMENT_API_KEY": key}, false},
		{"switch on, payment-rpc configured", map[string]string{"APP_ORDERING_REQUIRE_PAYMENT": "true", "APP_PAYMENT_UPSTREAM": "http://payment-rpc:80", "APP_PAYMENT_API_KEY": key}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("APP_CONFIG", "/dev/null")
			t.Setenv("APP_CATALOG_UPSTREAM", "http://catalog-rpc:80")
			t.Setenv("APP_CATALOG_API_KEY", serviceKey)
			t.Setenv("APP_HTTP_ADDR", ":0")
			t.Setenv("APP_LOG_LEVEL", "error")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cfg, err := config.Load()
			if err == nil {
				var cleanup func()
				_, cleanup, err = Initialize(cfg)
				if err == nil {
					cleanup()
				}
			}
			if tc.start && err != nil {
				t.Fatalf("rpc refused to start: %v", err)
			}
			if !tc.start && err == nil {
				t.Fatal("rpc started on a payment configuration it must refuse")
			}
			if err != nil && strings.Contains(err.Error(), key) {
				t.Fatalf("the refusal carries the key: %v", err)
			}
		})
	}
}
