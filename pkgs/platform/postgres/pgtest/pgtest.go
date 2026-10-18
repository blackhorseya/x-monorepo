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
//   - set, empty: fails. An empty opt-in is a typo, not "no opt-in".
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

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// EnvURL is the opt-in: point it at an existing database to bypass the container.
const EnvURL = "TEST_DATABASE_URL"

// Image is pinned to what deployments/docker/docker-compose.yaml runs locally,
// so the tests exercise the same major version as the dev stack.
const Image = "postgres:17-alpine"

var (
	once      sync.Once
	container *postgres.PostgresContainer
	url       string
	startErr  error
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

// Main wraps m.Run so the container started by URL is terminated after the
// package's tests finish. It returns the exit code for os.Exit. A package
// that also needs another helper's container calls m.Run and each helper's
// Terminate itself.
func Main(m *testing.M) int {
	code := m.Run()
	Terminate()
	return code
}

// Terminate stops the container URL started, if any. Idempotent.
func Terminate() {
	if container == nil {
		return
	}
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
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

	container, err = postgres.Run(c, Image,
		postgres.WithDatabase("test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		postgres.BasicWaitStrategies(),
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
