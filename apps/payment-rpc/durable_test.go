package main

import (
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

const (
	durableKey    = "payment-rpc-key-0123456789ab"
	durablePGPass = "s3cretpgpass"
	otherKey      = "console-user-key-0123456789"
)

// outcome of one run(args) under a given environment.
type outcome struct {
	err    error // run's error when it returned on its own
	served bool  // it answered /api/readyz 200 (and was then told to stop)
	// timedOut: it neither returned nor answered readyz 200 within the limit. Never a success: a
	// start that was expected to serve has failed, and a refusal that was expected has not happened.
	timedOut bool
}

// awaitReady watches a starting process: it returns when run returned (err), when ready() says
// readyz answered 200 (served), or when limit passes (timedOut). It stops nothing itself.
func awaitReady(done <-chan error, ready func() bool, limit time.Duration) outcome {
	deadline := time.Now().Add(limit)
	for {
		select {
		case err := <-done:
			return outcome{err: err}
		default:
		}
		if ready() {
			return outcome{served: true}
		}
		if time.Now().After(deadline) {
			return outcome{timedOut: true}
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// served only when readyz answered 200, timed out when it never did, in a start with nothing listening.
func TestAwaitReady(t *testing.T) {
	never := func() bool { return false }
	if out := awaitReady(make(chan error), never, 50*time.Millisecond); !out.timedOut || out.served || out.err != nil {
		t.Fatalf("never ready = %+v, want timedOut only", out)
	}
	if out := awaitReady(make(chan error), func() bool { return true }, time.Second); !out.served || out.timedOut {
		t.Fatalf("ready = %+v, want served only", out)
	}
	done := make(chan error, 1)
	done <- errors.New("refused")
	if out := awaitReady(done, never, time.Second); out.err == nil || out.served || out.timedOut {
		t.Fatalf("returned = %+v, want err only", out)
	}
}

// start runs the real entry point with args and env. A process that comes up is stopped with
// SIGTERM and reported as served, so a refusal that does not happen fails the test instead of
// hanging it. env maps APP_* names to values; a nil entry value means "unset".
func start(t *testing.T, args []string, env map[string]*string) outcome {
	t.Helper()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	defer signal.Stop(sig)

	addr := freeAddr(t)
	t.Setenv("APP_HTTP_ADDR", addr)
	t.Setenv("APP_LOG_LEVEL", "error")
	t.Setenv("APP_HTTP_SHUTDOWN_DELAY", "0")
	for k, v := range env {
		if v == nil {
			old, had := os.LookupEnv(k)
			_ = os.Unsetenv(k)
			t.Cleanup(func() {
				if had {
					_ = os.Setenv(k, old)
				}
			})
			continue
		}
		t.Setenv(k, *v)
	}

	done := make(chan error, 1)
	go func() { done <- run(args) }()
	ready := func() bool {
		res, err := http.Get("http://" + addr + server.ReadyzPath)
		if err != nil {
			return false
		}
		_ = res.Body.Close()

		return res.StatusCode == http.StatusOK
	}
	out := awaitReady(done, ready, 6*time.Second)
	if out.err != nil || (!out.served && !out.timedOut) {
		return out // run returned on its own (a refusal, or a clean exit)
	}
	// it is still running: stop it, repeatedly (a signal sent before NotifyContext was installed is lost)
	for {
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		select {
		case <-done:
			return out
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func s(v string) *string { return &v }

// durableEnv is a configuration --require-durable accepts; each case overrides one thing.
func durableEnv(t *testing.T, override map[string]*string) map[string]*string {
	t.Helper()
	pg := freshDB(t).URL
	env := map[string]*string{
		"APP_POSTGRES_URL":  s(pg),
		"APP_AUTH_MODE":     s("required"),
		"APP_AUTH_API_KEYS": s("rpc=" + durableKey + ":admin"),
	}
	for k, v := range override {
		env[k] = v
	}

	return env
}

// --require-durable is the deployment's last line: whatever the final effective settings are (env
// beats the config file, a Secret can hold an empty value), a payment-rpc that would serve payments
// from memory or without required auth must not start. The refusal never prints a secret.
func TestRun_RequireDurable(t *testing.T) {
	flag := []string{"serve", "--require-durable"}

	refused := map[string]map[string]*string{
		"postgres url empty":   {"APP_POSTGRES_URL": s("")},
		"postgres url unset":   {"APP_POSTGRES_URL": nil},
		"postgres url blank":   {"APP_POSTGRES_URL": s("   ")},
		"auth off":             {"APP_AUTH_MODE": s("off")},
		"auth unset (default)": {"APP_AUTH_MODE": nil},
		"auth optional":        {"APP_AUTH_MODE": s("optional")},
		// The service key contract: exactly one key, named rpc. A Secret shared with rpc's user keys
		// (console, ops...) must not authenticate those users against payment-rpc.
		"api keys: rpc and a user key":   {"APP_AUTH_API_KEYS": s("rpc=" + durableKey + ":admin,console=" + otherKey + ":admin")},
		"api keys: only a user key":      {"APP_AUTH_API_KEYS": s("console=" + otherKey + ":admin")},
		"api keys: a user key first":     {"APP_AUTH_API_KEYS": s("console=" + otherKey + ":admin,rpc=" + durableKey + ":admin")},
		"api keys: rpc twice":            {"APP_AUTH_API_KEYS": s("rpc=" + durableKey + ":admin,rpc=" + otherKey + ":admin")},
		"api keys: name is not rpc":      {"APP_AUTH_API_KEYS": s("payment=" + durableKey + ":admin")},
		"api keys: name differs in case": {"APP_AUTH_API_KEYS": s("RPC=" + durableKey + ":admin")},
		"api keys: none":                 {"APP_AUTH_API_KEYS": nil},
		"auth off with a key set":        {"APP_AUTH_MODE": s("off"), "APP_AUTH_API_KEYS": s("rpc=" + durableKey + ":admin")},
	}
	for name, override := range refused {
		t.Run("refuses "+name, func(t *testing.T) {
			out := start(t, flag, durableEnv(t, override))
			if out.served {
				t.Fatal("payment-rpc --require-durable started on a non-durable configuration")
			}
			if out.err == nil || !strings.Contains(out.err.Error(), "require-durable") {
				t.Fatalf("run = %v, want an error naming --require-durable", out.err)
			}
			for _, secret := range []string{durableKey, otherKey, durablePGPass} {
				if strings.Contains(out.err.Error(), secret) {
					t.Fatalf("the refusal prints a secret: %v", out.err)
				}
			}
		})
	}

	t.Run("accepts a durable configuration", func(t *testing.T) {
		out := start(t, flag, durableEnv(t, nil))
		if !out.served || out.err != nil {
			t.Fatalf("run = %+v, want a clean start and stop", out)
		}
	})

	t.Run("accepts the single rpc key with the platform's own spacing", func(t *testing.T) {
		out := start(t, flag, durableEnv(t, map[string]*string{"APP_AUTH_API_KEYS": s(" rpc = " + durableKey + " : admin , ")}))
		if !out.served || out.err != nil {
			t.Fatalf("run = %+v, want a clean start and stop", out)
		}
	})

	// The default is unchanged: no flag, the same non-durable settings still start (local dev).
	t.Run("without the flag it still starts in memory", func(t *testing.T) {
		out := start(t, []string{"serve"}, durableEnv(t, map[string]*string{"APP_POSTGRES_URL": s(""), "APP_AUTH_MODE": s("off")}))
		if !out.served || out.err != nil {
			t.Fatalf("run = %+v, want a start without the flag", out)
		}
	})

	// An unknown argument fails closed: a typo such as --require-durabel must not start the
	// service without the guarantee the operator thought they asked for.
	t.Run("refuses an unknown argument", func(t *testing.T) {
		out := start(t, []string{"serve", "--require-durabel"}, durableEnv(t, nil))
		if out.served || out.err == nil {
			t.Fatalf("run = %+v, want a refusal for an unknown argument", out)
		}
	})
}
