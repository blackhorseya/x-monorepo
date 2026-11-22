package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/cucumber/godog"
	"github.com/jackc/pgx/v5"

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
// (the admin surface under auth.mode=required) are the only ones for now.
var registers = []acceptance.Register{registerPlatformSteps}

// adminKey is the API key (role admin) the scenarios' rpc accepts.
const adminKey = "acceptance-admin-key-0123456789"

// TestAcceptance runs the .feature files under acceptance/features (Gherkin in zh-TW) against the
// real rpc (in this process, over HTTP) and a real Postgres: one database per scenario, copied from a
// template with every migration applied.
func TestAcceptance(t *testing.T) {
	tpl := newTemplate(t)

	var ran atomic.Int32
	suite := godog.TestSuite{
		ScenarioInitializer: func(sc *godog.ScenarioContext) {
			env := &acceptance.Env{AdminKey: adminKey}
			sc.Before(func(c context.Context, _ *godog.Scenario) (context.Context, error) {
				ran.Add(1)
				name, err := tpl.clone(c)
				if err != nil {
					return c, err
				}
				env.DatabaseURL = tpl.urlFor(name)
				return c, nil
			})
			sc.After(func(c context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
				return c, tpl.drop(c, env.DatabaseURL)
			})
			for _, register := range registers {
				register(sc, env)
			}
		},
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"acceptance/features"},
			TestingT: t,
			Strict:   true,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("acceptance: a scenario failed (see the output above)")
	}
	// A green run of nothing proves nothing: godog passes when the paths hold no scenario.
	if ran.Load() == 0 {
		t.Fatal("acceptance: no scenario ran; check acceptance/features")
	}
}

// ── per-scenario databases ───────────────────────────────────────────────────

// template is the migrated database every scenario's own database is copied from. All names carry a
// random run id, so two runs on the same server (-count=2, another package, another agent) never meet,
// and only what this run created is dropped.
type template struct {
	t    *testing.T
	conn *pgx.Conn // maintenance connection of the server (the database pgtest.URL points at)
	base *url.URL
	name string
	run  string
}

func randomID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// newTemplate creates the template database, applies apps/migrate to it, and registers its removal.
func newTemplate(t *testing.T) *template {
	t.Helper()
	base, err := url.Parse(pgtest.URL(t))
	if err != nil {
		t.Fatalf("acceptance: parse database url: %v", err)
	}
	tpl := &template{t: t, conn: pgtest.Conn(t, base.String()), base: base, run: randomID(t)}
	tpl.name = "acc_tpl_" + tpl.run
	c := context.Background()

	// Cleanups run last in, first out: the template's removal is registered after the connection, so it
	// runs while the connection is still open, and its error fails the test.
	if _, err := tpl.conn.Exec(c, "CREATE DATABASE "+pgx.Identifier{tpl.name}.Sanitize()); err != nil {
		t.Fatalf("acceptance: create template database (the role needs CREATEDB when TEST_DATABASE_URL is used): %v", err)
	}
	t.Cleanup(func() {
		if _, err := tpl.conn.Exec(c, "DROP DATABASE "+pgx.Identifier{tpl.name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("acceptance: drop template database %s: %v", tpl.name, err)
		}
	})

	// apps/migrate holds the list of every BC's migrations and does not export it, so build and run it
	// (apps/worker's TestRun_SIGTERM does the same).
	bin := buildApp(t, t.TempDir(), "migrate")
	runStep(t, "migrate up", budget(t, migrateLimit), "", childEnv("APP_POSTGRES_URL="+tpl.urlFor(tpl.name), "APP_LOG_LEVEL=error"), bin, "up")

	// CREATE DATABASE ... TEMPLATE fails while any session is connected to the template. Nothing connects to it
	// from here on, and the migrate process is gone; assert that instead of hoping.
	if _, err := tpl.conn.Exec(c, "ALTER DATABASE "+pgx.Identifier{tpl.name}.Sanitize()+" WITH ALLOW_CONNECTIONS false"); err != nil {
		t.Fatalf("acceptance: lock template database: %v", err)
	}
	var sessions int
	if err := tpl.conn.QueryRow(c, "SELECT count(*) FROM pg_stat_activity WHERE datname = $1", tpl.name).Scan(&sessions); err != nil {
		t.Fatalf("acceptance: count template sessions: %v", err)
	}
	if sessions != 0 {
		t.Fatalf("acceptance: template database %s still has %d session(s) after migrate up", tpl.name, sessions)
	}
	return tpl
}

func (x *template) urlFor(db string) string {
	u := *x.base
	u.Path = "/" + db
	u.RawPath = ""
	return u.String()
}

var scenarioSeq atomic.Int64

// clone makes one scenario's database from the template and returns its name.
func (x *template) clone(c context.Context) (string, error) {
	name := fmt.Sprintf("acc_%s_%d", x.run, scenarioSeq.Add(1))
	q := "CREATE DATABASE " + pgx.Identifier{name}.Sanitize() + " TEMPLATE " + pgx.Identifier{x.name}.Sanitize()
	if _, err := x.conn.Exec(c, q); err != nil {
		return "", fmt.Errorf("create scenario database %s: %w", name, err)
	}
	return name, nil
}

// drop removes the scenario database named by its url; the error is the scenario's failure. The rpc has
// been stopped by then, and WITH (FORCE) ends any session a failing step left behind.
func (x *template) drop(c context.Context, dbURL string) error {
	u, err := url.Parse(dbURL)
	if err != nil {
		return fmt.Errorf("drop scenario database: parse url: %w", err)
	}
	if len(u.Path) < 2 {
		return fmt.Errorf("drop scenario database: no database name in %q", dbURL)
	}
	name := u.Path[1:]
	if _, err := x.conn.Exec(c, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("drop scenario database %s: %w", name, err)
	}
	return nil
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
	sc.After(func(c context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		// The rpc goes first: its pool must be closed before the scenario's database is dropped (the
		// runner's After hook, registered earlier, runs after this one).
		w.shutdown()
		return c, nil
	})
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
func TestAcceptance_WorkerProcess(t *testing.T) {
	tpl := newTemplate(t)
	c := context.Background()
	name, err := tpl.clone(c)
	if err != nil {
		t.Fatal(err)
	}
	dbURL := tpl.urlFor(name)
	t.Cleanup(func() {
		if err := tpl.drop(c, dbURL); err != nil {
			t.Error(err)
		}
	})

	bin := buildApp(t, t.TempDir(), "worker")
	w := startWorker(t, bin, dbURL)
	w.stop(t)
}
