package main

import (
	"net"
	"net/http"
	"os"
	"os/signal"
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
	go func() { done <- run() }()

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

// The real entry point refuses a legacy auth.jwt: catalog-rpc authenticates
// static auth.api_keys only, so a configured JWT secret must stop the process
// before it serves, with or without keys beside it. A process that started
// anyway is told to stop, and the test fails.
func TestRun_RefusesLegacyJWT(t *testing.T) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	defer signal.Stop(sig)

	for name, apiKeys := range map[string]string{
		"jwt only":         "",
		"jwt with api key": "ops=ops-key-0123456789abc:admin",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := freshDB(t)
			addr := freeAddr(t)
			t.Setenv("APP_POSTGRES_URL", cfg.URL)
			t.Setenv("APP_HTTP_ADDR", addr)
			t.Setenv("APP_LOG_LEVEL", "error")
			t.Setenv("APP_HTTP_SHUTDOWN_DELAY", "0")
			t.Setenv("APP_AUTH_MODE", "required")
			t.Setenv("APP_AUTH_API_KEYS", apiKeys)
			t.Setenv("APP_AUTH_JWT_HS256_SECRET", "0123456789abcdef0123456789abcdef")

			done := make(chan error, 1)
			go func() { done <- run() }()

			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "auth.jwt") {
					t.Fatalf("run = %v, want an error naming auth.jwt", err)
				}
			case <-time.After(5 * time.Second):
				_ = syscall.Kill(os.Getpid(), syscall.SIGTERM) // it started: stop it
				<-done
				t.Fatal("run kept serving with auth.jwt configured, want a refused start")
			}
		})
	}
}
