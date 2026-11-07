// Package pgtest hands integration tests a Postgres to run against.
//
// Three states, decided by the presence of TEST_DATABASE_URL, not its value:
//
//   - unset: a throwaway postgres container (testcontainers) is started once
//     per test binary and torn down by Main. Only "no container runtime
//     reachable" skips, and only outside CI; everything after the runtime
//     answered (pull, start, readiness) is a failure.
//   - set, non-empty: that database is used as-is. Unreachable → the test
//     fails, never skips: the caller asked for it.
//     Tests that isolate themselves with CREATE DATABASE (pkgs/identity's
//     lock-expiry matrix) need that URL's role to hold CREATEDB
//     (ALTER ROLE <role> CREATEDB); without it they fail with SQLSTATE 42501.
//   - set, empty: fails. An empty opt-in is a typo, not "no opt-in".
//
// The container is also cleaned up when the test binary does not end through Main (#481). On Podman Ryuk, testcontainers'
// own reaper, is off, so nothing else would: with Main, a SIGINT or SIGTERM terminates it at once (exit 128+signal;
// ctrreap.OnSignal, installed by Main, not by URL: a package whose tests send SIGTERM to their own process to exercise a
// graceful shutdown must not have it, so it calls m.Run and Terminate itself and is covered by the next start only);
// go test -timeout, a panic or SIGKILL leave it, and the NEXT start of any pgtest or redistest container on the machine
// removes it (ctrreap.ReapOnce). Only containers labelled by this repo whose test process is gone are removed; one whose
// process is alive (another agent's tests on the same machine), or that carries no such label, is never touched. See
// package ctrreap for the table of ways a binary can end and what covers each. Nothing is reaped if no test ever runs
// again on the machine. Neither ~/.testcontainers.properties nor Ryuk is touched.
//
// Each package that wants a database adds its own TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }
//
// and calls URL(t) from the tests that need it. The TestMain is deliberately
// per package rather than hidden in a shared init: a package that forgets it
// leaks its container, and the leak points at that package.
package pgtest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/ctrreap"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

// EnvURL is the opt-in: point it at an existing database to bypass the container.
const EnvURL = "TEST_DATABASE_URL"

// Image is pinned to what deployments/docker/docker-compose.yaml runs locally,
// so the tests exercise the same major version as the dev stack.
const Image = "postgres:17-alpine"

var (
	once     sync.Once
	url      string
	startErr error

	// mu guards container: start runs under it (a signal that arrives while the container is still starting waits for
	// the start, up to the signal handler's grace) and so does terminate, which the signal handler runs from its own goroutine.
	mu        sync.Mutex
	container *postgres.PostgresContainer
)

// URL returns a connection string for a database the test may freely wipe.
// See the package comment for when it skips or fails.
func URL(t testing.TB) string {
	t.Helper()
	if v, ok := os.LookupEnv(EnvURL); ok {
		if v == "" {
			t.Fatalf("%s is set but empty: unset it for a container, or point it at a database", EnvURL)
		}
		return v
	}

	once.Do(start)
	if startErr != nil {
		var unreachable *runtimeUnreachableError
		if errors.As(startErr, &unreachable) && os.Getenv("CI") == "" {
			t.Skipf("%v (set %s to use an existing database)", startErr, EnvURL)
		}
		t.Fatal(startErr)
	}
	return url
}

// Telemetry returns quiet providers for a test's pool and shuts them down with
// t.Cleanup. Call it before DB: cleanups run last-in first-out, so the providers
// outlive the pool they instrument.
func Telemetry(t testing.TB) *otel.Providers {
	t.Helper()
	tel, shutdown, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatalf("pgtest: telemetry: %v", err)
	}
	t.Cleanup(shutdown)
	return tel
}

// DB opens a pool with cfg and closes it with t.Cleanup. The caller gets no
// closer, so there is nothing to defer: a defer would close the pool when the
// test function returns, before any t.Cleanup the test registered, and a purge
// registered with t.Cleanup would then run on a closed pool (#371).
//
// Because the close is registered here, before the caller's own t.Cleanup calls,
// last-in first-out runs the caller's cleanups first, with the pool still open:
//
//	db := pgtest.DB(t, config.Postgres{URL: pgtest.URL(t)}, pgtest.Telemetry(t))
//	t.Cleanup(func() {
//		if err := db.Exec("DELETE FROM t WHERE ...").Error; err != nil {
//			t.Errorf("purge: %v", err)
//		}
//	})
//
// A test does not open a pool itself: archtest.RunTestCleanup (every module's
// pgtest_cleanup_test.go) fails on any mention of NewDB, sql.Open, pgxpool.* or
// pgx.Connect* in a *_test.go file, and on Close of the sql.DB behind this pool.
// This package is where they are allowed.
func DB(t testing.TB, cfg config.Postgres, tel *otel.Providers) *gorm.DB {
	t.Helper()
	db, err := TryDB(t, cfg, tel)
	if err != nil {
		t.Fatalf("pgtest: open pool: %v", err)
	}
	return db
}

// TryDB is DB for a test that expects the open to be able to fail (a rejected
// login, a bad host, an expect_target mismatch) or that wants to word the failure
// itself. On success the pool is closed with t.Cleanup; on error nothing was opened
// and there is nothing to close. The error is the one NewDB returned, untouched.
func TryDB(t testing.TB, cfg config.Postgres, tel *otel.Providers) (*gorm.DB, error) {
	t.Helper()
	db, closePool, err := platformpg.NewDB(cfg, tel)
	if err != nil {
		return nil, err // NewDB returns a nil closer on error: register nothing
	}
	t.Cleanup(closePool)
	return db, nil
}

// TryDBContext is TryDB for the deadline-bound entry (postgres.NewDBContext): connecting and the first ping
// run under c, and a statement running when c ends is cancelled on the server. Same close and error contract as TryDB.
func TryDBContext(t testing.TB, c context.Context, cfg config.Postgres, tel *otel.Providers) (*gorm.DB, error) {
	t.Helper()
	db, closePool, err := platformpg.NewDBContext(c, cfg, tel)
	if err != nil {
		return nil, err
	}
	t.Cleanup(closePool)
	return db, nil
}

// ClosePool closes db now, for a test that needs the "pool already closed" state
// (a purge or a probe run against a dead pool, to see how it fails). The close
// registered by DB then finds it closed, which is a no-op.
func ClosePool(t testing.TB, db *gorm.DB) {
	t.Helper()
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("pgtest: pool handle: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("pgtest: close pool: %v", err)
	}
}

// Conn opens one pgx connection to url and closes it with t.Cleanup. A test that
// closes it earlier on purpose (to end a transaction it holds) may: pgx.Conn.Close
// is idempotent.
func Conn(t testing.TB, url string) *pgx.Conn {
	t.Helper()
	c := context.Background()
	conn, err := pgx.Connect(c, url)
	if err != nil {
		t.Fatalf("pgtest: connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(c) })
	return conn
}

// Main wraps m.Run so the container started by URL is terminated after the
// package's tests finish. It returns the exit code for os.Exit. A package
// that also needs another helper's container calls m.Run and each helper's
// Terminate itself.
func Main(m *testing.M) int {
	ctrreap.OnSignal(terminate)
	code := m.Run()
	Terminate()
	return code
}

// Terminate stops the container URL started, if any. Idempotent.
func Terminate() {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	terminate(c)
}

// terminate is Terminate under a context of the caller's (the signal handler's deadline).
func terminate(c context.Context) {
	mu.Lock()
	defer mu.Unlock()
	if container == nil {
		return
	}
	if err := container.Terminate(c); err != nil {
		fmt.Fprintf(os.Stderr, "pgtest: terminate container: %v\n", err)
	}
	container = nil
}

// runtimeUnreachableError is the one condition URL may skip on: no Docker /
// Podman API answered at all, so the machine simply has no container runtime.
type runtimeUnreachableError struct{ err error }

func (e *runtimeUnreachableError) Error() string {
	return "container runtime unreachable: " + e.err.Error()
}
func (e *runtimeUnreachableError) Unwrap() error { return e.err }

func start() {
	mu.Lock()
	defer mu.Unlock()

	c, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Probe the runtime first so "no Docker here" is distinguishable from
	// "Docker is here but the container failed": only the former may skip.
	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		startErr = &runtimeUnreachableError{err: err}
		return
	}
	if _, err := provider.DaemonHost(c); err != nil {
		startErr = &runtimeUnreachableError{err: err}
		return
	}
	if err := provider.Health(c); err != nil {
		startErr = &runtimeUnreachableError{err: err}
		return
	}

	// containers of test processes that are gone are removed before a new one starts; a failure here is a warning, not an error
	ctrreap.ReapOnce(c, provider.Client(), os.Stderr)

	container, err = postgres.Run(c, Image,
		postgres.WithDatabase("test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		postgres.BasicWaitStrategies(),
		testcontainers.WithLabels(ctrreap.Labels()),
	)
	if err != nil {
		startErr = fmt.Errorf("start %s: %w", Image, err)
		return
	}
	url, err = container.ConnectionString(c, "sslmode=disable")
	if err != nil {
		startErr = fmt.Errorf("postgres connection string: %w", err)
	}
}
