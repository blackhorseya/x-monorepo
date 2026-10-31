package main

import (
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// The real entry point under SIGTERM: run() serves on the configured port,
// readiness is 200, the signal makes it return nil (exit 0) and release the
// port. The drain itself (readyz 503 first, in-flight requests finishing,
// shutdown_timeout) is tested on a real socket in platform/server.
func TestRun_SIGTERM(t *testing.T) {
	// Without a handler of its own the test binary would die on the signal
	// if it arrived before run() installed NotifyContext.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	defer signal.Stop(sig)

	cfg := freshDB(t)
	addr := freeAddr(t)
	t.Setenv("APP_POSTGRES_URL", cfg.URL)
	t.Setenv("APP_HTTP_ADDR", addr)
	t.Setenv("APP_LOG_LEVEL", "error")
	t.Setenv("APP_HTTP_SHUTDOWN_DELAY", "0")

	done := make(chan error, 1)
	go func() { done <- run(nil) }()

	base := "http://" + addr
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := http.Get(base + server.ReadyzPath)
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("catalog-rpc never became ready on %s: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Repeated: a signal sent before NotifyContext was installed is lost.
	for {
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("run after SIGTERM = %v, want nil", err)
			}
			if _, err := http.Get(base + server.HealthzPath); err == nil {
				t.Fatal("port still answers after run returned")
			}
			return
		case <-time.After(500 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("run did not return after SIGTERM")
		}
	}
}

// freeAddr returns a 127.0.0.1 address with a port nothing listens on right now.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

// The real entry point refuses a legacy auth.jwt or auth.oidc: catalog-rpc
// authenticates static auth.api_keys only, so either must stop the process
// before it serves, with or without keys beside it. A process that started
// anyway is told to stop, and the test fails.
func TestRun_RefusesUnsupportedCredentialSources(t *testing.T) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	defer signal.Stop(sig)

	for name, c := range map[string]struct{ apiKeys, env, value, want string }{
		"jwt only":          {"", "APP_AUTH_JWT_HS256_SECRET", "0123456789abcdef0123456789abcdef", "auth.jwt"},
		"jwt with api key":  {"ops=ops-key-0123456789abc:admin", "APP_AUTH_JWT_HS256_SECRET", "0123456789abcdef0123456789abcdef", "auth.jwt"},
		"oidc only":         {"", "APP_AUTH_OIDC_ENABLED", "true", "auth.oidc"},
		"oidc with api key": {"ops=ops-key-0123456789abc:admin", "APP_AUTH_OIDC_ENABLED", "true", "auth.oidc"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := freshDB(t)
			addr := freeAddr(t)
			t.Setenv("APP_POSTGRES_URL", cfg.URL)
			t.Setenv("APP_HTTP_ADDR", addr)
			t.Setenv("APP_LOG_LEVEL", "error")
			t.Setenv("APP_HTTP_SHUTDOWN_DELAY", "0")
			t.Setenv("APP_AUTH_MODE", "required")
			t.Setenv("APP_AUTH_API_KEYS", c.apiKeys)
			if c.env == "APP_AUTH_OIDC_ENABLED" {
				// config.Load already insists on a well-formed issuer list, which
				// only a file can carry: give it one, so the refusal under test is
				// catalog-rpc's own and not the loader's.
				file := filepath.Join(t.TempDir(), "config.yaml")
				yaml := "auth:\n  oidc:\n    enabled: true\n    issuers:\n      - issuer: https://idp.example.com\n        audience: catalog-rpc\n"
				if err := os.WriteFile(file, []byte(yaml), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("APP_CONFIG", file)
			} else {
				t.Setenv(c.env, c.value)
			}

			done := make(chan error, 1)
			go func() { done <- run(nil) }()

			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("run = %v, want an error naming %s", err, c.want)
				}
			case <-time.After(5 * time.Second):
				_ = syscall.Kill(os.Getpid(), syscall.SIGTERM) // it started: stop it
				<-done
				t.Fatal("run kept serving with an unsupported credential source configured, want a refused start")
			}
		})
	}
}
