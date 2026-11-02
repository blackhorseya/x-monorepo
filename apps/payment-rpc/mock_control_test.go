package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	paymentbc "github.com/blackhorseya/x-monorepo/pkgs/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	paymentpg "github.com/blackhorseya/x-monorepo/pkgs/payment/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
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
	// the mock's tables are shared by every test of the package: empty before, and after (a fault left set
	// would fail the next test's provider)
	cleanMock := func() {
		if err := e.db.Exec("TRUNCATE payment.mock_checkouts, payment.mock_faults CASCADE").Error; err != nil {
			t.Errorf("truncate the mock tables: %v", err)
		}
	}
	cleanMock()
	t.Cleanup(cleanMock)
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

// holdTableLock takes an ACCESS EXCLUSIVE lock on a mock table from a connection of the test's own until released (or
// the test ends): every statement on that table waits, reads included. The release is idempotent and registered as a
// cleanup.
func holdTableLock(t *testing.T, url, table string) (release func()) {
	t.Helper()
	conn := pgtest.Conn(t, url)
	ctx := context.Background()
	if _, err := conn.Exec(ctx, `BEGIN`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `LOCK TABLE payment.`+table+` IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			if _, err := conn.Exec(ctx, `ROLLBACK`); err != nil {
				t.Errorf("release the table lock: %v", err)
			}
			if err := conn.Close(ctx); err != nil {
				t.Errorf("close the lock holder's connection: %v", err)
			}
		})
	}
	t.Cleanup(release)

	return release
}

// waitingOn reports how many backends wait on a lock while running a statement that mentions what.
func (e *consumeEnv) waitingOn(what string) int {
	e.t.Helper()

	return e.count(`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query ILIKE '%' || ? || '%' AND pid <> pg_backend_pid()`, what)
}

// The command's own deadline (#406): every subcommand is declared to end within mockTimeout, whole command. Each case
// makes the database wait on a table lock another transaction holds, through the real entry; the anchor is
// pg_stat_activity showing the command's statement waiting; at the deadline the command must return the cancellation,
// the statement must be over on the SERVER (not merely abandoned by the client), and the write it was waiting to
// make must NOT happen when the lock is let go. The positive control: with nothing in the way the same command succeeds
// inside the same budget, so a timeout means the lock, not a budget too short for the work.
func TestMockControl_EverySubcommandEndsAtItsDeadlineWhileTheDatabaseWaits(t *testing.T) {
	old := mockTimeout
	mockTimeout = 2 * time.Second
	t.Cleanup(func() { mockTimeout = old })

	type tc struct {
		name  string
		table string                               // the table whose lock the database waits on
		args  func(ref string) []string            // the command
		wrote func(e *consumeEnv, ref string) bool // did the write the command was waiting to make happen?
	}
	cases := []tc{
		{"pay", "mock_checkouts", func(string) []string { return []string{"pay", "--order", "ord-1"} },
			func(e *consumeEnv, ref string) bool { return e.paidAtProvider(ref) }},
		{"notify", "mock_checkouts", func(ref string) []string {
			return []string{"notify", "--ref", ref, "--id", "e1", "--type", "payment.captured"}
		},
			func(*consumeEnv, string) bool { return false }}, // a read: it signs, it writes nothing — only the deadline is asserted
		{"fail", "mock_faults", func(string) []string { return []string{"fail", "on", "--op", "get_payment"} },
			func(e *consumeEnv, _ string) bool { return e.count(`SELECT count(*) FROM payment.mock_faults`) > 0 }},
	}
	for _, c := range cases {
		t.Run(c.name+"/positive control: nothing in the way", func(t *testing.T) {
			e := newControlEnv(t)
			p := e.pendingWithCheckout()
			start := time.Now()
			if _, err := mockRun(t, c.args(p.ProviderRef())...); err != nil {
				t.Fatalf("mock %s with nothing in the way: %v", c.name, err)
			}
			if took := time.Since(start); took > mockTimeout {
				t.Fatalf("the unobstructed %s took %s, longer than the %s budget this test shrinks it to", c.name, took, mockTimeout)
			}
			if c.name != "notify" && !c.wrote(e, p.ProviderRef()) {
				t.Fatalf("the unobstructed %s did not make its write", c.name)
			}
		})

		t.Run(c.name+"/the database waits", func(t *testing.T) {
			e := newControlEnv(t)
			p := e.pendingWithCheckout()
			release := holdTableLock(t, e.pg.URL, c.table)

			type result struct {
				err  error
				took time.Duration
			}
			done := make(chan result, 1)
			start := time.Now()
			go func() {
				_, err := mockRun(t, c.args(p.ProviderRef())...)
				done <- result{err, time.Since(start)}
			}()
			var got *result
			t.Cleanup(func() {
				release()
				if got == nil {
					<-done
				}
			}) // the waiter is joined whatever happens

			deadline := time.Now().Add(10 * time.Second)
			for e.waitingOn(c.table) == 0 {
				if time.Now().After(deadline) {
					t.Fatal("fixture: the command never waited on the held table")
				}
				time.Sleep(20 * time.Millisecond)
			}

			var r result
			select {
			case r = <-done:
				got = &r
			case <-time.After(mockTimeout + 3*time.Second):
				release() // let it finish so the test can report instead of hanging
				r = <-done
				got = &r
				t.Fatalf("the command was still waiting %s after its %s deadline and then returned %v once the lock was released", time.Since(start), mockTimeout, r.err)
			}
			cancelled := r.err != nil && (errors.Is(r.err, context.DeadlineExceeded) || strings.Contains(r.err.Error(), "deadline") || strings.Contains(r.err.Error(), "canceled"))
			if !cancelled {
				t.Fatalf("at the deadline the command must return the cancellation, got %v", r.err)
			}
			if r.took > mockTimeout+3*time.Second {
				t.Fatalf("returned after %s, deadline %s", r.took, mockTimeout)
			}
			// the statement is over on the SERVER too, not only abandoned by the client
			if n := e.waitingOn(c.table); n != 0 {
				t.Fatalf("the command reported its failure while %d backend(s) still wait to make its write", n)
			}

			release()
			time.Sleep(500 * time.Millisecond) // a write that was merely abandoned would land now
			if c.wrote(e, p.ProviderRef()) {
				t.Fatal("the write the command gave up on went through when the lock was released")
			}
		})
	}
}

// waitUntil polls cond every 10ms until it holds; it fails the test with msg when limit passes first. It is how a
// test anchors on "the database is really waiting right now" before it acts.
func waitUntil(t *testing.T, limit time.Duration, msg string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(end) {
			t.Fatal(msg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// One deadline for the whole command (#406 r3): the budget is spent across SEVERAL database steps, and the deadline
// is the command's, not each step's. `mock pay` looks the payment up (payment.payments), then writes the payment to
// the provider (payment.mock_checkouts). The case waits on the first for about half the budget, lets it go, and
// waits on the second until the budget is over. At the deadline the command must have returned the cancellation AND
// the second statement must be over on the SERVER; a relative per-statement timeout started at the beginning would
// still be running (and would make the write when the lock is let go). The positive control: the same slow lookup
// with nothing in the way of the write succeeds inside the same budget, so the delay alone is not what fails.
func TestMockControl_DeadlineIsOneForTheWholeCommand_SlowLookupThenWrite(t *testing.T) {
	old := mockTimeout
	mockTimeout = 3 * time.Second
	t.Cleanup(func() { mockTimeout = old })
	const lookupDelay = 1600 * time.Millisecond

	t.Run("positive control: a slow lookup alone fits the budget", func(t *testing.T) {
		e := newControlEnv(t)
		p := e.pendingWithCheckout()
		release := holdTableLock(t, e.pg.URL, "payments")
		errc := make(chan error, 1)
		go func() {
			_, err := mockRun(t, "pay", "--order", "ord-1")
			errc <- err
		}()
		waitUntil(t, 4*time.Second, "fixture: the command never waited on payment.payments", func() bool { return e.waitingOn("payments") > 0 })
		time.Sleep(lookupDelay)
		release()
		select {
		case err := <-errc:
			if err != nil {
				t.Fatalf("a lookup that waited %s inside a %s budget must succeed: %v", lookupDelay, mockTimeout, err)
			}
		case <-time.After(mockTimeout + 3*time.Second):
			t.Fatal("the command did not return")
		}
		if !e.paidAtProvider(p.ProviderRef()) {
			t.Fatal("the slow-lookup command did not make its write")
		}
	})

	t.Run("the write waits after the lookup did", func(t *testing.T) {
		e := newControlEnv(t)
		p := e.pendingWithCheckout()
		releaseLookup := holdTableLock(t, e.pg.URL, "payments")
		releaseWrite := holdTableLock(t, e.pg.URL, "mock_checkouts")
		type result struct {
			err  error
			took time.Duration
		}
		done := make(chan result, 1)
		start := time.Now()
		go func() {
			_, err := mockRun(t, "pay", "--order", "ord-1")
			done <- result{err, time.Since(start)}
		}()
		var got *result
		t.Cleanup(func() {
			releaseLookup()
			releaseWrite()
			if got == nil {
				<-done
			}
		}) // the command is joined whatever happens

		// anchor 1: this call's lookup waits on payments; anchor 2: after it is let go, its write waits on mock_checkouts
		waitUntil(t, 4*time.Second, "fixture: the command never waited on payment.payments", func() bool { return e.waitingOn("payments") > 0 })
		time.Sleep(lookupDelay)
		releaseLookup()
		waitUntil(t, 4*time.Second, "fixture: the command never reached its write on payment.mock_checkouts", func() bool { return e.waitingOn("mock_checkouts") > 0 })

		var r result
		select {
		case r = <-done:
			got = &r
		case <-time.After(mockTimeout + 3*time.Second):
			releaseWrite()
			r = <-done
			got = &r
			t.Fatalf("the command was still waiting %s after its %s deadline and returned %v once the lock was released", time.Since(start), mockTimeout, r.err)
		}
		if r.err == nil || !(errors.Is(r.err, context.DeadlineExceeded) || strings.Contains(r.err.Error(), "deadline")) {
			t.Fatalf("at the deadline the command must return the deadline, got %v", r.err)
		}
		if r.took > mockTimeout+time.Second {
			t.Fatalf("returned after %s, deadline %s", r.took, mockTimeout)
		}
		// the statement is over on the SERVER when the command reports, not only abandoned by the client
		if n := e.waitingOn("mock_checkouts"); n != 0 {
			t.Fatalf("the command reported its deadline while %d backend(s) still wait to make its write (a per-statement timeout started at the beginning outlives the deadline)", n)
		}
		releaseWrite()
		time.Sleep(500 * time.Millisecond) // a write that was merely abandoned would land now
		if e.paidAtProvider(p.ProviderRef()) {
			t.Fatal("the write the command gave up on went through when the lock was released")
		}
	})
}

// startupPeer is a Postgres peer that accepts one connection, reads the StartupMessage, and never answers: a
// connection whose handshake does not complete. The StartupMessage it read (its protocol number is the anchor that
// the command's own call reached it, not some other process) is reported on started.
type startupPeer struct {
	url     string
	started chan struct{}
	mu      sync.Mutex
	conn    net.Conn
}

func newStartupPeer(t *testing.T) *startupPeer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &startupPeer{started: make(chan struct{}), url: "postgres://synthetic:synthetic@" + ln.Addr().String() + "/test?sslmode=disable"}
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		p.conn = c
		p.mu.Unlock()
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		hdr := make([]byte, 8)
		if _, err := io.ReadFull(c, hdr); err != nil {
			return
		}
		if binary.BigEndian.Uint32(hdr[4:]) != 196608 { // protocol 3.0: a StartupMessage, not an SSLRequest
			return
		}
		close(p.started)
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-acceptDone
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.conn != nil {
			_ = p.conn.Close()
		}
	})

	return p
}

// closedByPeerSide reports whether the accepted connection is closed from the command's side within limit: a read
// that ends in EOF or a reset means the command closed it; one that only times out means it is still open.
func (p *startupPeer) closedByPeerSide(limit time.Duration) bool {
	p.mu.Lock()
	c := p.conn
	p.mu.Unlock()
	_ = c.SetReadDeadline(time.Now().Add(limit))
	buf := make([]byte, 4096)
	for {
		_, err := c.Read(buf)
		if err == nil {
			continue
		}
		var ne net.Error
		return !(errors.As(err, &ne) && ne.Timeout())
	}
}

// One deadline reaches the initialization too (#406 r3): a peer that took the StartupMessage and never answers holds
// the command's connect (and the ping after it). With a 300ms budget the command returns the deadline; the connection
// it opened must be closed by then, not left open by an abandoned goroutine. The anchor is the StartupMessage read
// off this call's own connection; the positive control is the unobstructed command in the tests above.
func TestMockControl_DeadlineClosesAConnectionWhoseHandshakeNeverCompletes(t *testing.T) {
	old := mockTimeout
	mockTimeout = 300 * time.Millisecond
	t.Cleanup(func() { mockTimeout = old })
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, config.EnvPrefix+"_") {
			t.Setenv(k, "")
		}
	}
	peer := newStartupPeer(t)
	t.Setenv("APP_POSTGRES_URL", peer.url)
	t.Setenv("APP_LOG_LEVEL", "error")
	t.Setenv("APP_PAYMENT_PROVIDER", "mock")
	t.Setenv("APP_PAYMENT_MOCK_CONTROL", "true")
	t.Setenv("APP_PAYMENT_MOCK_WEBHOOK_SECRET", mockSecret)

	errc := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := mockRun(t, "fail", "on")
		errc <- err
	}()
	var err error
	finished := false
	t.Cleanup(func() {
		if !finished {
			select {
			case <-errc:
			case <-time.After(3 * time.Second):
				t.Error("the command did not return after the test finished")
			}
		}
	})
	select {
	case <-peer.started:
	case <-time.After(5 * time.Second):
		t.Fatal("fixture: the command never sent its StartupMessage to the peer")
	}
	select {
	case err = <-errc:
		finished = true
	case <-time.After(mockTimeout + 3*time.Second):
		t.Fatalf("the command was still running %s after its %s deadline", time.Since(start), mockTimeout)
	}
	if err == nil || !(errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "deadline")) {
		t.Fatalf("at the deadline the command must return the deadline, got %v", err)
	}
	if !peer.closedByPeerSide(500 * time.Millisecond) {
		t.Fatal("the command returned its deadline but the connection it opened is still open")
	}
}
