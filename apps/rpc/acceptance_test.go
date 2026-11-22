package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
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

// platformFeatures is the platform's own features (the admin surface under auth.mode=required).
const platformFeatures = "acceptance/features"

// repoRoot is the repository root, from this package's directory.
const repoRoot = "../.."

// registrations is every bounded context's acceptance, one per BC in go.work; the platform's own steps are not here.
// The runner calls each Register once per scenario, and fails when this list and go.work's BCs differ (checkRegistered).
// `task new-bc` adds a line at the anchor. A Register never adds an After hook: what a step starts goes to env.Defer,
// which the runner's one After hook closes before it drops the database.
var registrations = []registration{
	// new-bc:acceptance
}

// adminKey is the API key (role admin) the scenarios' rpc accepts.
const adminKey = "acceptance-admin-key-0123456789"

// testReserve is what a step's context leaves out of the test's time: 20% of what is left when the run starts, at least 5s,
// so the closers, the database drops and the container shutdown still fit before go test's own -timeout.
func testReserve(left time.Duration) time.Duration { return max(left/5, 5*time.Second) }

// TestAcceptance runs the .feature files (Gherkin in zh-TW) against the real rpc (in this process, over HTTP) and a
// real Postgres: one database per scenario, copied from a template with every migration applied. The platform's are
// under acceptance/features; each bounded context's are under pkgs/<bc>/acceptance/features.
func TestAcceptance(t *testing.T) {
	bcs, err := bcsFromWork(repoRoot)
	if err != nil {
		t.Fatalf("acceptance: %v", err)
	}
	platform := scanFeatures(t, platformFeatures)
	if platform.scenarios == 0 {
		t.Fatalf("acceptance: %s holds no scenario", platformFeatures)
	}
	dirs := []string{platformFeatures}
	want, workers := platform.scenarios, platform.worker
	perBC := map[string]int{}
	for _, bc := range bcs {
		dir := filepath.Join(repoRoot, "pkgs", bc, "acceptance", "features")
		st := scanFeatures(t, dir)
		perBC[bc] = st.scenarios
		want += st.scenarios
		workers += st.worker
		dirs = append(dirs, dir)
	}
	if err := checkRegistered(bcs, registrations, perBC); err != nil {
		t.Fatalf("acceptance: %v", err)
	}

	// What a step may use of the test's time: its context ends before go test's own -timeout.
	var stepDeadline time.Time
	if d, ok := t.Deadline(); ok {
		stepDeadline = d.Add(-testReserve(time.Until(d)))
	}
	lim := limitsFor(t)
	var workerBin string
	if workers > 0 {
		workerBin = buildApp(t, t.TempDir(), "worker")
	}
	tpl := newTemplate(t)
	tpl.prepare(want)

	var ran atomic.Int32
	suite := godog.TestSuite{
		ScenarioInitializer: func(sc *godog.ScenarioContext) {
			env := &acceptance.Env{AdminKey: adminKey}
			sc.Before(func(c context.Context, s *godog.Scenario) (context.Context, error) {
				ran.Add(1)
				dbURL, err := tpl.take()
				if err != nil {
					return c, err
				}
				env.DatabaseURL = dbURL
				if !stepDeadline.IsZero() {
					var cancel context.CancelFunc
					c, cancel = context.WithDeadline(c, stepDeadline)
					env.Defer(func() error { cancel(); return nil })
				}
				if err := startRPC(env); err != nil {
					return c, err
				}
				if hasTag(s, workerTag) {
					pid, err := startScenarioWorker(env, workerBin, lim)
					if err != nil {
						return c, err
					}
					godog.Logf(c, "worker started as pid %d", pid)
				}
				return c, nil
			})
			// The only After hook: godog runs them in registration order, so two hooks would be an order nobody
			// chose. What the runner and the steps registered with env.Defer (the worker, the rpc and its pool, the
			// deadline) is closed first, last registered first; then the scenario's database is dropped, and drop
			// fails the scenario if anything is still connected.
			sc.After(func(c context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
				closeErr := env.Close()
				dropErr := tpl.drop(context.WithoutCancel(c), env.DatabaseURL)
				return c, errors.Join(closeErr, dropErr)
			})
			registerPlatformSteps(sc, env)
			for _, r := range registrations {
				r.register(sc, env)
			}
		},
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    dirs,
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
		t.Fatalf("acceptance: %d scenario(s) ran, the feature files hold %d", got, want)
	}
}

func hasTag(s *godog.Scenario, name string) bool {
	for _, tag := range s.Tags {
		if tag.Name == name {
			return true
		}
	}
	return false
}

// startRPC starts the rpc in this process (Initialize + httptest), as TestPlatform_E2E does: auth.mode=required, one admin
// API key, the scenario's database. Its pool and server go on env's stack, closed before the database is dropped.
func startRPC(env *acceptance.Env) error {
	srv, cleanup, err := Initialize(&config.AppConfig{
		HTTP:     config.HTTP{Addr: ":0"},
		Log:      config.Log{Level: "error", Format: "text"},
		Postgres: config.Postgres{URL: env.DatabaseURL},
		Auth:     config.Auth{Mode: config.AuthRequired, APIKeys: "acceptance=" + env.AdminKey + ":admin"},
	})
	if err != nil {
		return fmt.Errorf("start rpc: %w", err)
	}
	ts := httptest.NewServer(srv.Handler())
	env.RPCURL = ts.URL
	env.Defer(func() error { ts.Close(); cleanup(); return nil })

	return nil
}

// startScenarioWorker starts an apps/worker subprocess on the scenario's database for a scenario tagged @worker. It is
// stopped gracefully when env closes: SIGTERM, exit code 0 and its "worker stopped" line are part of the scenario's result.
func startScenarioWorker(env *acceptance.Env, bin string, lim workerLimits) (pid int, err error) {
	w, err := launchWorker(bin, env.DatabaseURL, lim)
	if err != nil {
		return 0, err
	}
	env.Defer(func() error {
		defer w.killAndReap()
		return w.shutdown(lim.stop)
	})

	return w.cmd.Process.Pid, nil
}

// ── the platform's steps ─────────────────────────────────────────────────────

// platformWorld is one scenario's last call to the admin surface.
type platformWorld struct {
	env    *acceptance.Env
	client adminv1connect.AdminServiceClient
	resp   *adminv1.ListPoisonResponse
	err    error
}

// registerPlatformSteps defines the steps of acceptance/features/admin_auth.feature. The first one is also the
// "given" every bounded context's feature uses: the runner has already started the rpc.
func registerPlatformSteps(sc *godog.ScenarioContext, env *acceptance.Env) {
	w := &platformWorld{env: env}
	sc.Step(`^rpc 以 auth\.mode=required 啟動，資料庫為該場景獨立的 DB$`, w.rpcIsUp)
	sc.Step(`^以 admin API key 呼叫 ListPoison$`, w.listPoisonAsAdmin)
	sc.Step(`^不帶任何 credential 呼叫 ListPoison$`, w.listPoisonAnonymous)
	sc.Step(`^呼叫成功，回應是沒有任何 poison 訊息的清單$`, w.succeededWithNoMessages)
	sc.Step(`^指定不存在的 bounded context 時，由 handler 回 ([a-z_]+) 與 x-error-code "([^"]+)"$`, w.unknownBoundedContext)
	sc.Step(`^呼叫被拒絕，Connect code 為 "([^"]+)"，x-error-code 為 "([^"]+)"$`, w.rejected)
}

// rpcIsUp states the precondition the runner's Before hook set up (startRPC): the rpc answers at RPCURL, on this
// scenario's own database.
func (w *platformWorld) rpcIsUp() error {
	if w.env.RPCURL == "" || w.env.DatabaseURL == "" {
		return errors.New("the runner did not start the rpc on a scenario database")
	}
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
	return acceptance.ExpectRejected(err, code, errorCode)
}

func (w *platformWorld) rejected(code, errorCode string) error {
	if w.err == nil {
		return fmt.Errorf("ListPoison succeeded (%v), want %s / %s", w.resp, code, errorCode)
	}
	return acceptance.ExpectRejected(w.err, code, errorCode)
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
