package main

import (
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
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

	addr := freeAddr(t)
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
			t.Fatalf("rpc never became ready on %s: %v", addr, err)
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
