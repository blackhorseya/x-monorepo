package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	paymentbc "github.com/blackhorseya/x-monorepo/pkgs/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	paymentpg "github.com/blackhorseya/x-monorepo/pkgs/payment/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

// docs/payment.md §9 S6: `payment-rpc mock pay|notify|fail` is how a kind cluster (or a test) plays the customer
// and the gateway of the mock provider, from outside the three payment-rpc processes. It reads and writes the
// same payment.mock_* tables they do. It exists only when payment.provider is "mock" AND payment.mock_control is
// on, and it opens no route: the only way to the provider's side is this command, run by someone who can already
// reach the database.
//
// Every test here enters through run(), the way main does, with the settings in the environment as a pod has them.

const mockSecret = "mock-control-test-secret"

// controlEnv is a consumeEnv whose mock provider keeps its state in Postgres (like the three processes
// do), signs with mockSecret, and has the control entry's settings in the process environment.
func newControlEnv(t *testing.T) *consumeEnv {
	t.Helper()
	e := newConsumeEnv(t)
	e.prov = mock.New(mock.Options{Store: paymentpg.NewMockStore(e.db), Secret: []byte(mockSecret)})
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, config.EnvPrefix+"_") {
			t.Setenv(k, "")
		}
	}
	t.Setenv("APP_POSTGRES_URL", e.pg.URL)
	t.Setenv("APP_LOG_LEVEL", "error")
	t.Setenv("APP_PAYMENT_PROVIDER", "mock")
	t.Setenv("APP_PAYMENT_MOCK_CONTROL", "true")
	t.Setenv("APP_PAYMENT_MOCK_WEBHOOK_SECRET", mockSecret)

	return e
}

// mockRun runs `payment-rpc mock <args>` through run and returns what it printed on stdout.
func mockRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	runErr := run(append([]string{"mock"}, args...))
	os.Stdout = old
	_ = w.Close()
	out, _ := io.ReadAll(r)
	_ = r.Close()

	return string(out), runErr
}

// printed is what `mock pay` and `mock notify` print: one JSON object with the signed webhook to deliver.
type printed struct {
	Ref       string `json:"ref"`
	EventID   string `json:"event_id"`
	Body      string `json:"body"`
	Signature string `json:"signature"`
}

func decodePrinted(t *testing.T, out string) printed {
	t.Helper()
	var p printed
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &p); err != nil {
		t.Fatalf("the command did not print one JSON object: %v\n%s", err, out)
	}
	if p.Ref == "" || p.EventID == "" || p.Body == "" || p.Signature == "" {
		t.Fatalf("the printed delivery is incomplete: %+v", p)
	}

	return p
}

func (e *consumeEnv) paidAtProvider(ref string) bool {
	e.t.Helper()
	got, err := e.prov.GetPayment(context.Background(), ref)
	if err != nil {
		e.t.Fatal(err)
	}

	return got.Paid
}

// pay plays the customer: the checkout of the order becomes paid at the provider and the command prints the
// signed webhook a real provider would POST. The webhook verifies with the provider's own secret, names the
// checkout and carries the payment's amount.
func TestMockControl_PayPaysTheOrdersCheckoutAndSignsTheWebhook(t *testing.T) {
	e := newControlEnv(t)
	p := e.pendingWithCheckout()
	if e.paidAtProvider(p.ProviderRef()) {
		t.Fatal("setup: the checkout is paid before the command ran")
	}

	out, err := mockRun(t, "pay", "--order", "ord-1")
	if err != nil {
		t.Fatalf("mock pay: %v", err)
	}
	d := decodePrinted(t, out)
	if d.Ref != p.ProviderRef() {
		t.Fatalf("paid checkout %q, want the order's %q", d.Ref, p.ProviderRef())
	}
	if !e.paidAtProvider(p.ProviderRef()) {
		t.Fatal("the checkout is not paid at the provider after `mock pay`")
	}
	ev, err := e.prov.VerifyWebhook([]byte(d.Body), d.Signature)
	if err != nil {
		t.Fatalf("the printed webhook does not verify with the provider's secret: %v", err)
	}
	if ev.Type != mock.WebhookCaptured || ev.Ref != p.ProviderRef() || ev.Amount.Amount != 1000 || ev.Amount.Currency != "TWD" {
		t.Fatalf("webhook = %+v, want payment.captured for %s 1000 TWD", ev, p.ProviderRef())
	}
}

// --amount rehearses a customer who paid a different amount (T4, §3 the NEEDS_REVIEW rows).
func TestMockControl_PayCanPayADifferentAmount(t *testing.T) {
	e := newControlEnv(t)
	p := e.pendingWithCheckout()

	out, err := mockRun(t, "pay", "--order", "ord-1", "--amount", "999", "--currency", "TWD")
	if err != nil {
		t.Fatalf("mock pay --amount: %v", err)
	}
	d := decodePrinted(t, out)
	ev, err := e.prov.VerifyWebhook([]byte(d.Body), d.Signature)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Amount.Amount != 999 {
		t.Fatalf("webhook amount = %d, want 999", ev.Amount.Amount)
	}
	got, err := e.prov.GetPayment(context.Background(), p.ProviderRef())
	if err != nil || !got.Paid || got.Amount.Amount != 999 {
		t.Fatalf("provider says %+v (%v), want paid 999", got, err)
	}
}

// --deliver is the whole path: pay, then POST the signed webhook to the serve process's own route. The
// route stores and queues it exactly once; delivering the same notification again (redelivery) adds nothing.
func TestMockControl_PayDeliversToTheWebhookRoute(t *testing.T) {
	e := newControlEnv(t)
	e.pendingWithCheckout()
	base := e.serveWebhook()

	for range 2 {
		if _, err := mockRun(t, "pay", "--order", "ord-1", "--deliver", base+"/api/payment/webhook"); err != nil {
			t.Fatalf("mock pay --deliver: %v", err)
		}
	}
	if got := e.webhookRows(); got != 1 {
		t.Fatalf("%d webhook rows after delivering the same payment twice, want 1", got)
	}
	if got := e.webhookQueued(); got != 1 {
		t.Fatalf("%d WebhookReceived events queued, want 1", got)
	}
}

// A refused delivery is the command's failure: a script that waits for "delivered" must not read a 4xx as one.
func TestMockControl_DeliverFailsWhenTheRouteRefuses(t *testing.T) {
	e := newControlEnv(t)
	e.pendingWithCheckout()
	base := e.serveWebhook()
	t.Setenv("APP_PAYMENT_MOCK_WEBHOOK_SECRET", "not-the-secret-serve-signs-with") // the command signs with another secret

	_, err := mockRun(t, "pay", "--order", "ord-1", "--deliver", base+"/api/payment/webhook")
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("a webhook the route refuses (bad signature, 4xx) must fail the command with the status, got %v", err)
	}
	if got := e.webhookRows(); got != 0 {
		t.Fatalf("%d webhook rows from a refused delivery", got)
	}
}

// notify sends a webhook of any type for any id without moving the checkout: redelivery (same id, same
// bytes), another type (ignored by the route), and a delivery before or after the others (the caller orders).
func TestMockControl_NotifySignsAnyWebhookWithoutMovingTheCheckout(t *testing.T) {
	e := newControlEnv(t)
	p := e.pendingWithCheckout()

	args := []string{"notify", "--ref", p.ProviderRef(), "--id", "evt-redelivered", "--type", mock.WebhookCaptured, "--amount", "1000", "--currency", "TWD"}
	first, err := mockRun(t, args...)
	if err != nil {
		t.Fatalf("mock notify: %v", err)
	}
	second, err := mockRun(t, args...)
	if err != nil {
		t.Fatal(err)
	}
	a, b := decodePrinted(t, first), decodePrinted(t, second)
	if a.EventID != "evt-redelivered" || a != b {
		t.Fatalf("the same notify twice must print the same delivery (a redelivery), got %+v and %+v", a, b)
	}
	if ev, err := e.prov.VerifyWebhook([]byte(a.Body), a.Signature); err != nil || ev.ID != "evt-redelivered" || ev.Ref != p.ProviderRef() {
		t.Fatalf("notify output does not verify as the event asked for: %+v %v", ev, err)
	}
	if e.paidAtProvider(p.ProviderRef()) {
		t.Fatal("notify moved the checkout: only pay does that")
	}

	out, err := mockRun(t, "notify", "--ref", p.ProviderRef(), "--id", "evt-other", "--type", "payment.refund.updated", "--amount", "1000", "--currency", "TWD")
	if err != nil {
		t.Fatal(err)
	}
	if ev, err := e.prov.VerifyWebhook([]byte(decodePrinted(t, out).Body), decodePrinted(t, out).Signature); err != nil || ev.Type != "payment.refund.updated" {
		t.Fatalf("a notify of another type must carry that type: %+v %v", ev, err)
	}
}

// fail makes the provider's calls fail in every process that shares the database — the three payment-rpc
// workloads — until `fail off`. Faults are read by a provider only where payment.mock_control is on, so a
// production mock (control off) never reads a stray row.
func TestMockControl_FailMakesTheProviderUnavailableInEveryProcessUntilOff(t *testing.T) {
	e := newControlEnv(t)
	p := e.pendingWithCheckout()
	db := e.db
	other := func(control bool) port.PaymentProvider { // another process's provider over the same database
		pr, err := paymentbc.ProvidePaymentProvider(config.Payment{
			Provider: "mock", MockControl: control, MockWebhookSecret: mockSecret, Window: 30 * time.Minute,
			ReconcileWindow: 24 * time.Hour, MockCheckoutTTL: 30 * time.Minute, ExpireInterval: 15 * time.Minute, ExpireBatch: 100,
		}, db)
		if err != nil {
			t.Fatal(err)
		}

		return pr
	}
	query := func(pr port.PaymentProvider) error {
		_, err := pr.GetPayment(context.Background(), p.ProviderRef())

		return err
	}
	if err := query(other(true)); err != nil {
		t.Fatalf("setup: the provider fails before any fault was set: %v", err)
	}

	if _, err := mockRun(t, "fail", "on"); err != nil {
		t.Fatalf("mock fail on: %v", err)
	}
	if err := query(other(true)); !errors.Is(err, port.ErrProviderUnavailable) {
		t.Fatalf("a provider with control on answered after `fail on`: %v", err)
	}
	if err := query(other(false)); err != nil {
		t.Fatalf("a provider with control OFF honoured a fault row: %v", err)
	}

	if _, err := mockRun(t, "fail", "off"); err != nil {
		t.Fatalf("mock fail off: %v", err)
	}
	if err := query(other(true)); err != nil {
		t.Fatalf("the provider still fails after `fail off`: %v", err)
	}
}

// --op limits the fault to some calls: the outage the §3 table needs is "cannot be asked" (get_payment) while
// a cancel or refund still works, or the other way round.
func TestMockControl_FailCanTargetOneOperation(t *testing.T) {
	e := newControlEnv(t)
	p := e.pendingWithCheckout()
	pr, err := paymentbc.ProvidePaymentProvider(config.Payment{
		Provider: "mock", MockControl: true, MockWebhookSecret: mockSecret, Window: 30 * time.Minute,
		ReconcileWindow: 24 * time.Hour, MockCheckoutTTL: 30 * time.Minute, ExpireInterval: 15 * time.Minute, ExpireBatch: 100,
	}, e.db)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = mockRun(t, "fail", "on", "--op", "get_payment"); err != nil {
		t.Fatal(err)
	}
	if _, err = pr.GetPayment(context.Background(), p.ProviderRef()); !errors.Is(err, port.ErrProviderUnavailable) {
		t.Fatalf("get_payment answered with the fault on: %v", err)
	}
	if err = pr.CancelCheckout(context.Background(), p.ProviderRef()); err != nil {
		t.Fatalf("cancel_checkout failed though only get_payment was faulted: %v", err)
	}
	if _, err = mockRun(t, "fail", "off", "--op", "get_payment"); err != nil {
		t.Fatal(err)
	}
	if _, err = pr.GetPayment(context.Background(), p.ProviderRef()); err != nil {
		t.Fatalf("get_payment still fails after `fail off --op get_payment`: %v", err)
	}
}

// The command exists only when asked for. Each refusal names the setting to change and changes nothing.
func TestMockControl_RefusesUnlessTheProviderIsTheMockAndControlIsOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"control not enabled (the default)", map[string]string{"APP_PAYMENT_MOCK_CONTROL": ""}, "payment.mock_control"},
		{"control explicitly off", map[string]string{"APP_PAYMENT_MOCK_CONTROL": "false"}, "payment.mock_control"},
		{"another provider, control on", map[string]string{"APP_PAYMENT_PROVIDER": "stripe"}, "payment.provider"},
		{"control on but no signing secret", map[string]string{"APP_PAYMENT_MOCK_WEBHOOK_SECRET": ""}, "payment.mock_webhook_secret"},
		{"no database", map[string]string{"APP_POSTGRES_URL": ""}, "postgres.url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newControlEnv(t)
			p := e.pendingWithCheckout()
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			for _, args := range [][]string{{"pay", "--order", "ord-1"}, {"notify", "--ref", p.ProviderRef(), "--id", "e", "--type", "t"}, {"fail", "on"}} {
				out, err := mockRun(t, args...)
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("`mock %s` must be refused naming %q, got %v", args[0], tc.want, err)
				}
				if strings.TrimSpace(out) != "" {
					t.Fatalf("a refused `mock %s` printed a delivery: %s", args[0], out)
				}
			}
			if e.paidAtProvider(p.ProviderRef()) {
				t.Fatal("a refused `mock pay` paid the checkout")
			}
			if n := e.count(`SELECT count(*) FROM payment.mock_faults`); n != 0 {
				t.Fatalf("a refused `mock fail` left %d fault rows", n)
			}
		})
	}
}

// Unknown input is an error, never a quiet success a script would read as done.
func TestMockControl_RejectsWhatItCannotDo(t *testing.T) {
	e := newControlEnv(t)
	e.pendingWithCheckout()

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no subcommand", nil, "notify"},
		{"unknown subcommand", []string{"refund"}, "refund"},
		{"pay for an order with no payment", []string{"pay", "--order", "no-such-order"}, "no-such-order"},
		{"pay without saying what", []string{"pay"}, "--order"},
		{"pay with a negative amount", []string{"pay", "--order", "ord-1", "--amount", "-1", "--currency", "TWD"}, "amount"},
		{"fail without on/off", []string{"fail"}, "on"},
		{"fail on an operation the provider does not have", []string{"fail", "on", "--op", "launch_missiles"}, "launch_missiles"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := mockRun(t, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error naming %q, got %v (stdout %q)", tc.want, err, out)
			}
		})
	}
	if n := e.count(`SELECT count(*) FROM payment.mock_checkouts WHERE paid`); n != 0 {
		t.Fatalf("%d checkouts paid by commands that were all refused", n)
	}
}

// §1 / §7: no new route. With the control enabled, serve answers 404 on anything a control entry could have
// been, and the webhook route stays the only provider-facing path. (A pin: it holds before the command exists
// and must keep holding.)
func TestMockControl_OpensNoRoute(t *testing.T) {
	e := newControlEnv(t)
	base := e.serveWebhook()

	for _, path := range []string{"/api/payment/mock", "/api/payment/mock/pay", "/api/payment/mock/notify", "/api/payment/mock/fail", "/api/mock/pay"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			req, err := http.NewRequest(method, base+path, bytes.NewReader([]byte("{}")))
			if err != nil {
				t.Fatal(err)
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close()
			if res.StatusCode != http.StatusNotFound {
				t.Fatalf("%s %s = %d, want 404: the control entry must not be an HTTP route", method, path, res.StatusCode)
			}
		}
	}
}
