//go:build unix

package reaptest

import (
	"strings"
	"syscall"
	"testing"
	"time"
)

// readyBudget is how long a fixture gets to have its container up (image already pulled: seconds; a cold pull is longer).
const readyBudget = 90 * time.Second

// shortTimeout is the -test.timeout of the fixture that is meant to hit it. The container must be up well inside it,
// so the wait for it is shorter still (a fixture that is not ready by then is a setup failure, not a result).
// Sized for a loaded machine: with two packages running their cells at once under -race (about eight fixtures starting
// containers together on one Podman VM) a postgres container took over 30s to come up in 3 of 14 stress rounds, which
// is a "no container within 30s" SETUP FAILURE of the harness, never a mistaken removal.
const (
	shortTimeout     = 90 * time.Second
	shortReadyBudget = 60 * time.Second
)

// Scenarios runs, against the calling package's own test binary, one cell per way a test binary can end:
//
//	SIGTERM            the container is gone as soon as the process is (layer 1, the signal handler)
//	SIGKILL            the container outlives it; the NEXT start in any process removes it (layer 2)
//	panic              same
//	go test -timeout   same (the timeout's panic exits without returning from m.Run, like any panic)
//
// Each cell first proves its own precondition (the container exists, the owner ended abnormally, the container
// outlived it) and only then asserts the outcome, so a run that never had a container, or whose owner ended
// normally, fails as a SETUP failure instead of passing for the wrong reason.
func Scenarios(t *testing.T) {
	t.Helper()
	api := NeedRuntime(t)
	cs := On(t, api)
	t.Run("SIGTERM removes the container when the process ends", func(t *testing.T) {
		t.Parallel()
		sigterm(t, cs)
	})
	t.Run("SIGKILL leaves a container the next start removes", func(t *testing.T) {
		t.Parallel()
		orphan(t, cs, "sigkill")
	})
	t.Run("a panic leaves a container the next start removes", func(t *testing.T) {
		t.Parallel()
		orphan(t, cs, "panic")
	})
	t.Run("go test -timeout leaves a container the next start removes", func(t *testing.T) {
		t.Parallel()
		orphan(t, cs, "timeout")
	})
}

// up starts a hold fixture and returns it with the id of its container (found by the host port its URL names).
func up(t *testing.T, cs *Containers, o Opts, budget time.Duration) (*Child, string) {
	t.Helper()
	a := Spawn(t, o)
	u := a.WaitReady(budget)
	id := cs.IDByPort(PortOf(t, u))
	if id == "" {
		t.Fatalf("reaptest: SETUP FAILURE, not a result: no container publishes the port of %s", u)
	}
	t.Cleanup(func() { // a container the code under test left behind is removed here, by the id this test found itself
		if cs.Exists(id) {
			cs.Remove(id)
		}
	})
	return a, id
}

func sigterm(t *testing.T, cs *Containers) {
	a, id := up(t, cs, Opts{Namespace: Namespace(t)}, readyBudget)
	a.Signal(syscall.SIGTERM)
	if code := a.Wait(60 * time.Second); code == 0 {
		t.Errorf("the process exited 0 after SIGTERM: a signal must end it with a non-zero code\n%s", a.Output())
	}
	if !cs.GoneWithin(id, 5*time.Second) {
		t.Errorf("SIGTERM: the container %.12s is still there after the process ended", id)
	}
}

func orphan(t *testing.T, cs *Containers, how string) {
	ns := Namespace(t)
	o, budget := Opts{Namespace: ns}, readyBudget
	if how == "timeout" {
		o.Timeout, budget = shortTimeout, shortReadyBudget
	}
	a, id := up(t, cs, o, budget)

	var code int
	switch how {
	case "sigkill":
		a.Kill()
		code = -1
	case "panic":
		a.Panic()
		code = a.Wait(5 * time.Second)
	case "timeout":
		code = a.Wait(shortTimeout + 60*time.Second)
		if !strings.Contains(a.Output(), "test timed out") {
			t.Fatalf("reaptest: SETUP FAILURE, not a result: the fixture did not end by the -test.timeout panic:\n%s", a.Output())
		}
	}
	if code == 0 {
		t.Fatalf("reaptest: SETUP FAILURE, not a result: the owner ended normally (%s)", how)
	}
	// the precondition of "the next start removes it": the container outlived its owner
	if !cs.Exists(id) {
		t.Fatalf("reaptest: SETUP FAILURE, not a result: the container %.12s is already gone after the owner ended by %s, there is nothing for the next start to remove", id, how)
	}

	b := Spawn(t, Opts{Mode: "once", Namespace: ns})
	b.WaitReady(readyBudget)
	if code := b.Wait(60 * time.Second); code != 0 {
		t.Fatalf("reaptest: SETUP FAILURE, not a result: the next start exited %d:\n%s", code, b.Output())
	}
	if !cs.GoneWithin(id, 5*time.Second) {
		t.Errorf("%s: the container %.12s of the dead owner is still there after the next start", how, id)
	}
}
