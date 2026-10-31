package main

import (
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
	durableKey    = "catalog-rpc-key-0123456789ab"
	durablePGPass = "s3cretpgpass"
)

// outcome of one run(args) under a given environment.
type outcome struct {
	err    error // run's error when it returned on its own
	served bool  // it answered /api/readyz 200 (and was then told to stop)
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
	deadline := time.Now().Add(6 * time.Second)
	for {
		select {
		case err := <-done:
			return outcome{err: err}
		default:
		}
		if res, err := http.Get("http://" + addr + server.ReadyzPath); err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				for {
					_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
					select {
					case <-done:
						return outcome{served: true}
					case <-time.After(300 * time.Millisecond):
					}
				}
			}
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
			<-done
			return outcome{served: true}
		}
		time.Sleep(20 * time.Millisecond)
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
// beats the config file, a Secret can hold an empty value), a catalog-rpc that would serve prices
// from memory or without required auth must not start. The refusal never prints a secret.
func TestRun_RequireDurable(t *testing.T) {
	flag := []string{"--require-durable"}

	refused := map[string]map[string]*string{
		"postgres url empty":      {"APP_POSTGRES_URL": s("")},
		"postgres url unset":      {"APP_POSTGRES_URL": nil},
		"postgres url blank":      {"APP_POSTGRES_URL": s("   ")},
		"auth off":                {"APP_AUTH_MODE": s("off")},
		"auth unset (default)":    {"APP_AUTH_MODE": nil},
		"auth optional":           {"APP_AUTH_MODE": s("optional")},
		"auth off with a key set": {"APP_AUTH_MODE": s("off"), "APP_AUTH_API_KEYS": s("rpc=" + durableKey + ":admin")},
	}
	for name, override := range refused {
		t.Run("refuses "+name, func(t *testing.T) {
			out := start(t, flag, durableEnv(t, override))
			if out.served {
				t.Fatal("catalog-rpc --require-durable started on a non-durable configuration")
			}
			if out.err == nil || !strings.Contains(out.err.Error(), "require-durable") {
				t.Fatalf("run = %v, want an error naming --require-durable", out.err)
			}
			for _, secret := range []string{durableKey, durablePGPass} {
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

	// The default is unchanged: no flag, the same non-durable settings still start (local dev).
	t.Run("without the flag it still starts in memory", func(t *testing.T) {
		out := start(t, nil, durableEnv(t, map[string]*string{"APP_POSTGRES_URL": s(""), "APP_AUTH_MODE": s("off")}))
		if !out.served || out.err != nil {
			t.Fatalf("run = %+v, want a start without the flag", out)
		}
	})

	// An unknown argument fails closed: a typo such as --require-durabel must not start the
	// service without the guarantee the operator thought they asked for.
	t.Run("refuses an unknown argument", func(t *testing.T) {
		out := start(t, []string{"--require-durabel"}, durableEnv(t, nil))
		if out.served || out.err == nil {
			t.Fatalf("run = %+v, want a refusal for an unknown argument", out)
		}
	})
}
