package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	inventorypg "github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	orderingpg "github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// The real entry point under SIGTERM (pgtest): run(nil) starts the router
// and the signal makes it return nil (exit 0). The drain of an in-flight
// handler is tested against the real subscribers in platform/eventbus.
func TestRun_SIGTERM(t *testing.T) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM) // keeps the test binary alive if the signal beats NotifyContext
	defer signal.Stop(sig)

	url := pgtest.URL(t)
	c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, m := range []postgres.Migration{orderingpg.Migration(), inventorypg.Migration()} {
		if err := postgres.Up(c, config.Postgres{URL: url}, m); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("APP_POSTGRES_URL", url)
	t.Setenv("APP_LOG_LEVEL", "error")

	done := make(chan error, 1)
	go func() { done <- run(nil) }()

	// Let it get past wiring and into Run; a signal before NotifyContext is lost, hence the loop.
	time.Sleep(500 * time.Millisecond)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("run after SIGTERM = %v, want nil", err)
			}
			return
		case <-time.After(500 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("run did not return after SIGTERM")
		}
	}
}
