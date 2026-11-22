package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Child processes of the acceptance harness: apps/migrate (builds the template database) and
// apps/worker (started per scenario). One helper, waitChild, decides when a child is over.

// Budgets for the children. A child never gets more than what is left of the test (see budget).
const (
	buildBudget  = 2 * time.Minute  // go build of one app
	migrateLimit = 1 * time.Minute  // migrate up
	readyLimit   = 30 * time.Second // from start to the worker's readiness line
	stopLimit    = 15 * time.Second // from SIGTERM to exit
)

// budget is the time a child may take: max, and never more than half of what the test has left,
// so the steps after it still fit before go test's own -timeout. A stuck child must fail the test
// with its step named, not hang until the binary is killed.
func budget(t *testing.T, limit time.Duration) time.Duration {
	t.Helper()
	if d, ok := t.Deadline(); ok {
		return min(limit, time.Until(d)/2)
	}
	return limit
}

// outcome is why waitChild returned.
type outcome int

const (
	exited   outcome = iota // the process ended on its own; code is its exit code (non-zero is still "exited")
	failed                  // waiting itself failed (not an exit status): err says why
	timedOut                // the limit passed first; the child was killed and reaped
)

func (o outcome) String() string {
	switch o {
	case exited:
		return "exited"
	case failed:
		return "failed"
	default:
		return "timed out"
	}
}

type waitResult struct {
	outcome outcome
	code    int
	err     error
}

// child is one started process. done is closed once cmd.Wait has returned; err is its result, valid after done.
type child struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
	out  *syncBuffer
}

// startChild starts name with args in env (nothing inherited: the parent's APP_* would override
// what the test sets). Stdout and stderr are both kept in out; onLine sees each stdout line.
func startChild(dir, name string, env []string, onLine func(string), args ...string) (*child, error) {
	out := &syncBuffer{}
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = &lineWriter{out: out, onLine: onLine}
	cmd.Stderr = out
	cmd.WaitDelay = 5 * time.Second // a grandchild holding the pipes open must not hold the wait forever
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	c := &child{cmd: cmd, done: make(chan struct{}), out: out}
	// fire-and-forget: ends when the process does (waitChild and killAndReap kill it on every failure path).
	go func() {
		c.err = cmd.Wait()
		close(c.done)
	}()
	return c, nil
}

// waitChild returns when the child exits, when waiting fails, or when limit passes, and says which.
// On timeout it kills the child and waits until it is really gone before returning.
func waitChild(c *child, limit time.Duration) waitResult {
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-c.done:
		return resultOf(c.err)
	case <-timer.C:
		_ = c.cmd.Process.Kill()
		<-c.done
		return waitResult{outcome: timedOut, code: -1}
	}
}

func resultOf(err error) waitResult {
	if err == nil {
		return waitResult{outcome: exited, code: 0}
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return waitResult{outcome: exited, code: ee.ExitCode(), err: err}
	}
	return waitResult{outcome: failed, code: -1, err: err}
}

// runStep runs one short-lived child to its end: it must exit 0 within limit, else the test fails
// naming the step and printing what the child wrote.
func runStep(t *testing.T, step string, limit time.Duration, dir string, env []string, name string, args ...string) {
	t.Helper()
	c, err := startChild(dir, name, env, nil, args...)
	if err != nil {
		t.Fatalf("%s: %v", step, err)
	}
	res := waitChild(c, limit)
	if res.outcome != exited || res.code != 0 {
		t.Fatalf("%s: %s (exit code %d, limit %s): %v\n%s", step, res.outcome, res.code, limit.Round(time.Millisecond), res.err, c.out.String())
	}
}

// childEnv is the environment of a child: the parent's, minus every APP_* (a stray APP_CONFIG or
// APP_POSTGRES_URL must not leak in), plus extra.
func childEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "APP_") {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

// buildApp compiles one app of this repo into dir and returns the binary's path.
func buildApp(t *testing.T, dir, app string) string {
	t.Helper()
	bin := filepath.Join(dir, app)
	runStep(t, "build apps/"+app, budget(t, buildBudget), "", childEnv(),
		"go", "build", "-o", bin, "github.com/blackhorseya/x-monorepo/apps/"+app)
	return bin
}

// worker is a started apps/worker process.
type worker struct {
	*child
	ready   chan struct{}
	stopped chan struct{}
}

// Worker log lines are JSON (log.format=json) on stdout. app.go's Run logs "worker started" as its
// first act and "worker stopped" on the way out; those two messages are the signals.
const (
	workerStartedMsg = "worker started"
	workerStoppedMsg = "worker stopped"
)

// startWorker starts the worker binary against databaseURL and waits for its "worker started" line.
// The worker is killed and reaped at t's cleanup if the test did not stop it first.
func startWorker(t *testing.T, bin, databaseURL string) *worker {
	t.Helper()
	w := &worker{ready: make(chan struct{}), stopped: make(chan struct{})}
	var once1, once2 sync.Once
	onLine := func(line string) {
		var rec struct {
			Msg string `json:"msg"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil {
			return
		}
		switch rec.Msg {
		case workerStartedMsg:
			once1.Do(func() { close(w.ready) })
		case workerStoppedMsg:
			once2.Do(func() { close(w.stopped) })
		}
	}
	c, err := startChild("", bin, childEnv(
		"APP_POSTGRES_URL="+databaseURL,
		"APP_LOG_LEVEL=info",
		"APP_LOG_FORMAT=json",
	), onLine)
	if err != nil {
		t.Fatalf("worker setup: %v", err)
	}
	w.child = c
	t.Logf("worker started as pid %d", c.cmd.Process.Pid)
	t.Cleanup(func() { w.killAndReap() })

	// The readiness signal is the child's own line; it either comes, or the child ends first, or the limit passes.
	limit := budget(t, readyLimit)
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-w.ready:
		return w
	case <-c.done:
		res := resultOf(c.err)
		t.Fatalf("worker setup: %s before it was ready (exit code %d): %v\n%s", res.outcome, res.code, res.err, c.out.String())
	case <-timer.C:
		w.killAndReap()
		t.Fatalf("worker setup: no %q line within %s\n%s", workerStartedMsg, limit.Round(time.Millisecond), c.out.String())
	}
	return nil
}

// killAndReap kills the worker if it still runs and waits until it is gone. Safe to call twice.
func (w *worker) killAndReap() {
	_ = w.cmd.Process.Kill() // an already finished process: the error is ignored
	<-w.done
}

// stop sends SIGTERM and requires what a graceful stop is: an exit code of 0 AND the "worker stopped"
// line, within stopLimit. Either missing fails the test; the worker output is printed.
func (w *worker) stop(t *testing.T) {
	t.Helper()
	if err := w.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("worker stop: SIGTERM: %v\n%s", err, w.out.String())
	}
	limit := budget(t, stopLimit)
	res := waitChild(w.child, limit)
	if res.outcome != exited || res.code != 0 {
		t.Fatalf("worker stop: %s, exit code %d (want exited with 0, limit %s): %v\n%s", res.outcome, res.code, limit.Round(time.Millisecond), res.err, w.out.String())
	}
	select {
	case <-w.stopped:
	default:
		t.Fatalf("worker stop: exited 0 but never logged %q\n%s", workerStoppedMsg, w.out.String())
	}
}

// syncBuffer is a bytes.Buffer safe for the copying goroutine and the test to use at once.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// lineWriter keeps everything in out and calls onLine for each complete line.
type lineWriter struct {
	out    *syncBuffer
	onLine func(string)
	buf    []byte
}

func (l *lineWriter) Write(p []byte) (int, error) {
	n, err := l.out.Write(p)
	if l.onLine == nil {
		return n, err
	}
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		l.onLine(string(l.buf[:i]))
		l.buf = l.buf[i+1:]
	}
	return n, err
}
