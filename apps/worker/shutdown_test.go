package main

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// migrateUp applies every bounded context's migrations to the pgtest database (APP_POSTGRES_URL, already set), as
// the deploy order does before a worker starts (the worker refuses to start on a missing topic table, app.go).
// It runs apps/migrate itself, so the list of BCs stays in that one place; with no BC it applies nothing. Up is a
// no-op when current, so the shared pgtest database and -count=2 are fine.
func migrateUp(t *testing.T) {
	t.Helper()
	// One budget for both children, shorter than what the test binary has left (half of it, so run(nil) and the
	// SIGTERM loop below still fit), and never more than migrateBudget: a stuck build or database must fail here
	// with the step named, not hang until go test's own -timeout.
	budget := migrateBudget
	if d, ok := t.Deadline(); ok {
		budget = min(budget, time.Until(d)/2)
	}
	c, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	bin := filepath.Join(t.TempDir(), "migrate")
	runStep(t, c, budget, "build apps/migrate", "go", "build", "-o", bin, "github.com/blackhorseya/x-monorepo/apps/migrate")
	runStep(t, c, budget, "migrate up", bin, "up")
}

// migrateBudget caps the time migrateUp may spend on its two child processes.
const migrateBudget = 2 * time.Minute

// runStep runs one child under c: on timeout CommandContext kills it, WaitDelay bounds the wait for its output
// pipes (a grandchild such as the compiler can hold them open), and the failure says which step ran out of time.
func runStep(t *testing.T, c context.Context, budget time.Duration, step, name string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(c, name, args...)
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if c.Err() != nil {
		t.Fatalf("%s: did not finish within the %s budget: %v\n%s", step, budget.Round(time.Millisecond), err, out)
	}
	if err != nil {
		t.Fatalf("%s: %v\n%s", step, err, out)
	}
}

// The real entry point under SIGTERM (pgtest): run(nil) starts the router
// and the signal makes it return nil (exit 0). The drain of an in-flight
// handler is tested against the real subscribers in platform/eventbus.
func TestRun_SIGTERM(t *testing.T) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM) // keeps the test binary alive if the signal beats NotifyContext
	defer signal.Stop(sig)

	url := pgtest.URL(t)
	t.Setenv("APP_POSTGRES_URL", url)
	t.Setenv("APP_LOG_LEVEL", "error")
	migrateUp(t)

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
