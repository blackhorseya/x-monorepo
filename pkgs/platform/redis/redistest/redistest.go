// Package redistest hands integration tests a Redis, with the same contract
// as postgres/pgtest: TEST_REDIS_URL unset → a throwaway container, started
// once per test binary and terminated by Main; set and non-empty → used
// as-is, unreachable fails; set but empty → fails. Only "no container
// runtime reachable" skips, and only outside CI.
//
// Like pgtest's, the container is cleaned up when the test binary does not end through Main (#481): with Main, SIGINT /
// SIGTERM terminate it at once (exit 128+signal; a package that signals its own process in its tests calls m.Run and
// Terminate itself and has no handler), and go test -timeout, a panic or SIGKILL are cleaned up by the next start of
// any pgtest or redistest container on the machine, which removes only containers labelled by this repo whose test
// process is gone. See package ctrreap.
//
//	func TestMain(m *testing.M) { os.Exit(redistest.Main(m)) }
package redistest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/ctrreap"
)

// EnvURL is the opt-in: point it at an existing Redis to bypass the container.
const EnvURL = "TEST_REDIS_URL"

// Image is pinned to what deployments/docker/docker-compose.yaml runs locally.
const Image = "redis:7-alpine"

var (
	once     sync.Once
	url      string
	startErr error

	// mu guards container, as in pgtest.
	mu        sync.Mutex
	container *redis.RedisContainer
)

// URL returns a connection URL for a Redis the test may freely flush.
func URL(t testing.TB) string {
	t.Helper()
	if v, ok := os.LookupEnv(EnvURL); ok {
		if v == "" {
			t.Fatalf("%s is set but empty: unset it for a container, or point it at a redis", EnvURL)
		}
		return v
	}

	once.Do(start)
	if startErr != nil {
		var unreachable *runtimeUnreachableError
		if errors.As(startErr, &unreachable) && os.Getenv("CI") == "" {
			t.Skipf("%v (set %s to use an existing redis)", startErr, EnvURL)
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
		fmt.Fprintf(os.Stderr, "redistest: terminate container: %v\n", err)
	}
	container = nil
}

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

	ctrreap.ReapBeforeStart(c, provider.Client(), os.Stderr)

	container, err = redis.Run(c, Image, testcontainers.WithLabels(ctrreap.Labels()))
	if err != nil {
		startErr = fmt.Errorf("start %s: %w", Image, err)
		return
	}
	url, err = container.ConnectionString(c)
	if err != nil {
		startErr = fmt.Errorf("redis connection string: %w", err)
	}
}
