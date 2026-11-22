package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	gherkin "github.com/cucumber/gherkin/go/v42"
	"github.com/cucumber/godog"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/acceptance"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	adminv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/admin/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contract/admin/v1/adminv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server/servertest"
)

// TestMain: this package's tests send SIGTERM to their own process (shutdown_test.go), so it cannot use
// pgtest.Main (its signal handler would answer that SIGTERM); it ends the container itself.
func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Terminate()
	os.Exit(code)
}

// registers is every feature area's steps. The runner calls each once per scenario. The platform's own
// (the admin surface under auth.mode=required) are the only ones for now. A Register never adds an After
// hook: what a step starts goes to env.Defer, which the runner's one After hook closes before it drops the database.
var registers = []acceptance.Register{registerPlatformSteps}

// adminKey is the API key (role admin) the scenarios' rpc accepts.
const adminKey = "acceptance-admin-key-0123456789"

const featuresDir = "acceptance/features"

// TestAcceptance runs the .feature files under acceptance/features (Gherkin in zh-TW) against the
// real rpc (in this process, over HTTP) and a real Postgres: one database per scenario, copied from a
// template with every migration applied.
func TestAcceptance(t *testing.T) {
	want := countScenarios(t, featuresDir)
	if want == 0 {
		t.Fatalf("acceptance: %s holds no scenario", featuresDir)
	}
	tpl := newTemplate(t)
	tpl.prepare(want)

	var ran atomic.Int32
	suite := godog.TestSuite{
		ScenarioInitializer: func(sc *godog.ScenarioContext) {
			env := &acceptance.Env{AdminKey: adminKey}
			sc.Before(func(c context.Context, _ *godog.Scenario) (context.Context, error) {
				ran.Add(1)
				dbURL, err := tpl.take()
				if err != nil {
					return c, err
				}
				env.DatabaseURL = dbURL
				return c, nil
			})
			// The only After hook: godog runs them in registration order, so two hooks would be an order nobody
			// chose. What the steps registered with env.Defer (the rpc and its pool) is closed first, then the
			// scenario's database is dropped, and drop fails the scenario if anything is still connected.
			sc.After(func(c context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
				closeErr := env.Close()
				dropErr := tpl.drop(c, env.DatabaseURL)
				return c, errors.Join(closeErr, dropErr)
			})
			for _, register := range registers {
				register(sc, env)
			}
		},
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{featuresDir},
			TestingT: t,
			Strict:   true,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("acceptance: a scenario failed (see the output above)")
	}
	// A green run that ran fewer scenarios than the feature files hold proves less than it says: godog passes
	// when nothing ran. go test in CI has no -v, so this is what makes a green CI mean they all ran.
	if got := int(ran.Load()); got != want {
		t.Fatalf("acceptance: %d scenario(s) ran, the feature files under %s hold %d", got, featuresDir, want)
	}
}

// countScenarios is how many scenarios godog will run for the .feature files under dir: the pickles
// gherkin makes of them (an outline counts once per example row).
func countScenarios(t *testing.T, dir string) int {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.feature"))
	if err != nil {
		t.Fatal(err)
	}
	next := 0
	newID := func() string { next++; return fmt.Sprint(next) }
	total := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := gherkin.ParseGherkinDocument(strings.NewReader(string(raw)), newID)
		if err != nil {
			t.Fatalf("acceptance: parse %s: %v", f, err)
		}
		total += len(gherkin.Pickles(*doc, f, newID))
	}
	return total
}

// ── the platform's steps ─────────────────────────────────────────────────────

// platformWorld is one scenario's rpc and the last call's outcome.
type platformWorld struct {
	env    *acceptance.Env
	ts     *httptest.Server
	stop   func()
	client adminv1connect.AdminServiceClient
	resp   *adminv1.ListPoisonResponse
	err    error
}

// registerPlatformSteps defines the steps of acceptance/features/admin_auth.feature.
func registerPlatformSteps(sc *godog.ScenarioContext, env *acceptance.Env) {
	w := &platformWorld{env: env}
	sc.Step(`^rpc 以 auth\.mode=required 啟動，資料庫為該場景獨立的 DB$`, w.startRPC)
	sc.Step(`^以 admin API key 呼叫 ListPoison$`, w.listPoisonAsAdmin)
	sc.Step(`^不帶任何 credential 呼叫 ListPoison$`, w.listPoisonAnonymous)
	sc.Step(`^呼叫成功，回應是沒有任何 poison 訊息的清單$`, w.succeededWithNoMessages)
	sc.Step(`^指定不存在的 bounded context 時，由 handler 回 ([a-z_]+) 與 x-error-code "([^"]+)"$`, w.unknownBoundedContext)
	sc.Step(`^呼叫被拒絕，Connect code 為 "([^"]+)"，x-error-code 為 "([^"]+)"$`, w.rejected)
}

func (w *platformWorld) shutdown() {
	if w.ts != nil {
		w.ts.Close()
		w.ts = nil
	}
	if w.stop != nil {
		w.stop()
		w.stop = nil
	}
}

func (w *platformWorld) startRPC() error {
	if w.env.DatabaseURL == "" {
		return errors.New("the scenario has no database: the runner's Before hook did not run")
	}
	srv, cleanup, err := Initialize(&config.AppConfig{
		HTTP:     config.HTTP{Addr: ":0"},
		Log:      config.Log{Level: "error", Format: "text"},
		Postgres: config.Postgres{URL: w.env.DatabaseURL},
		Auth:     config.Auth{Mode: config.AuthRequired, APIKeys: "acceptance=" + w.env.AdminKey + ":admin"},
	})
	if err != nil {
		return fmt.Errorf("start rpc: %w", err)
	}
	w.stop = cleanup
	w.ts = httptest.NewServer(srv.Handler())
	w.env.Defer(func() error { w.shutdown(); return nil }) // before the runner drops the scenario's database
	w.env.RPCURL = w.ts.URL
	return nil
}

func (w *platformWorld) newClient(opts ...connect.ClientInterceptor) adminv1connect.AdminServiceClient {
	opts = append([]connect.ClientInterceptor{servertest.WithErrorMeta()}, opts...)
	return adminv1connect.NewAdminServiceClient(connect.NewClient(connecthttp.NewTransport(http.DefaultClient, w.env.RPCURL), opts...))
}

func (w *platformWorld) listPoisonAsAdmin(c context.Context) error {
	w.client = w.newClient(servertest.WithHeader(server.APIKeyHeader, w.env.AdminKey))
	w.resp, w.err = w.client.ListPoison(c, &adminv1.ListPoisonRequest{})
	return nil
}

func (w *platformWorld) listPoisonAnonymous(c context.Context) error {
	w.client = w.newClient()
	w.resp, w.err = w.client.ListPoison(c, &adminv1.ListPoisonRequest{})
	return nil
}

func (w *platformWorld) succeededWithNoMessages() error {
	if w.err != nil {
		return fmt.Errorf("ListPoison failed: %w", w.err)
	}
	if w.resp == nil {
		return errors.New("ListPoison returned no response and no error")
	}
	if n := len(w.resp.GetMessages()); n != 0 {
		return fmt.Errorf("ListPoison returned %d poison messages, want none (the template has no bounded context)", n)
	}
	return nil
}

// unknownBoundedContext shows the call reaches the handler's own logic: the handler, not an interceptor,
// answers a bounded context it does not know.
func (w *platformWorld) unknownBoundedContext(c context.Context, code, errorCode string) error {
	_, err := w.client.ListPoison(c, &adminv1.ListPoisonRequest{BoundedContext: "no-such-bc"})
	return checkRejected(err, code, errorCode)
}

func (w *platformWorld) rejected(code, errorCode string) error {
	if w.err == nil {
		return fmt.Errorf("ListPoison succeeded (%v), want %s / %s", w.resp, code, errorCode)
	}
	return checkRejected(w.err, code, errorCode)
}

// checkRejected asserts the Connect code and x-error-code the RPC actually answered, not an HTTP status.
func checkRejected(err error, code, errorCode string) error {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return fmt.Errorf("want a connect error %s / %s, got %w", code, errorCode, err)
	}
	if got := ce.Code().String(); got != code {
		return fmt.Errorf("connect code = %q, want %q: %w", got, code, err)
	}
	if got := servertest.ErrorMeta(err).Get("x-error-code"); got != errorCode {
		return fmt.Errorf("x-error-code = %q, want %q: %w", got, errorCode, err)
	}
	return nil
}

// ── the worker subprocess ────────────────────────────────────────────────────

// TestAcceptance_WorkerProcess: the harness can run apps/worker as a child against a scenario's database:
// it starts, logs its readiness line, and ends on SIGTERM with exit code 0 and its "worker stopped" line.
// The template has no bounded context, so the worker is idle; nothing here claims event consumption.
// The worker goes on the same Env stack a scenario's steps use, closed before the database is dropped.
func TestAcceptance_WorkerProcess(t *testing.T) {
	tpl := newTemplate(t)
	env := &acceptance.Env{DatabaseURL: tpl.make()}
	t.Cleanup(func() {
		if err := errors.Join(env.Close(), tpl.drop(context.Background(), env.DatabaseURL)); err != nil {
			t.Error(err)
		}
	})

	bin := buildApp(t, t.TempDir(), "worker")
	w := startWorker(t, env, bin)
	w.stop(t)
}
