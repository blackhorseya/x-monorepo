package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// These tests drive `task catalog-rpc` itself, the real entry point (#358/#361): the Taskfile
// decides which settings are pinned and which can still come from the parent environment, and
// neither a Go test of the config file nor lint nor CI's other jobs can see that. The Taskfile's
// own build dependency compiles the real catalog-rpc, so every case ends in the real config.Load.
//
// Contract under test (docs/operations.md, "本機開發"):
//   - APP_CONFIG, APP_HTTP_ADDR, APP_AUTH_MODE, APP_AUTH_API_KEYS are pinned by the task: an
//     inherited value changes nothing.
//   - every other APP_* (APP_POSTGRES_URL, APP_OTEL_*...) keeps env > file; a non-empty
//     CATALOG_DATABASE_URL beats both.
//   - CATALOG_CONFIG is one path argument: spaces are fine, a missing file is seeded from the
//     example, an existing file is never overwritten, a failing cp fails the task.

const (
	taskDevKey = "task-entry-test-key-0123456789abcdef"
	deadPG     = "postgres://app:app@127.0.0.1:1/app?sslmode=disable" // nothing listens on port 1
)

// syncBuf is a bytes.Buffer the child process and the test can share.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.String()
}

// childBudget is how long a child process may run: never more than max, and shorter than what is
// left of this test binary's own deadline. A `go test -timeout` panic runs no t.Cleanup, so the
// child has to be cancelled by its own clock before that alarm, not by the test finishing.
func childBudget(t *testing.T, max time.Duration) time.Duration {
	t.Helper()
	budget := max
	if d, ok := t.Deadline(); ok {
		if left := time.Until(d) * 3 / 4; left < budget {
			budget = left
		}
	}
	if budget <= 0 {
		t.Fatal("no time left before the test binary's deadline")
	}

	return budget
}

// boundedCommand is exec.Command with a deadline (childBudget): at the deadline the child's whole
// process group (task, its shell, what they started) is killed and Wait returns; WaitDelay bounds
// the wait for an output pipe a grandchild may still hold open.
func boundedCommand(t *testing.T, max time.Duration, dir, name string, args ...string) (*exec.Cmd, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), childBudget(t, max))
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // one group: kill -pgid reaches every descendant
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second

	return cmd, cancel
}

// taskRun is one `task catalog-rpc` run.
type taskRun struct {
	port   string
	out    *syncBuf
	done   chan error
	exited bool
	err    error
	cancel context.CancelFunc // the same cancellation the deadline uses: kills the whole group
}

// taskEnv: the child's parent environment, without anything of the developer's own
// APP_* / CATALOG_* (those would decide the outcome), plus extra.
func taskEnv(extra map[string]string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "APP_") || strings.HasPrefix(kv, "CATALOG_") {
			continue
		}
		env = append(env, kv)
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}

	return env
}

// startTask runs `task catalog-rpc` from the repo root with the given parent environment and
// CATALOG_CONFIG. The binary goes to a temp BIN_DIR and only catalog-rpc is built (APPS).
func startTask(t *testing.T, binDir, config string, env map[string]string) *taskRun {
	t.Helper()
	if _, err := exec.LookPath("task"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("task is not on PATH: CI's setup action installs it, this test must run there")
		}
		t.Skip("task is not on PATH")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(freeAddr(t))
	cmd, cancel := boundedCommand(t, 4*time.Minute, "", "task", "-d", root, "catalog-rpc",
		"BIN_DIR="+binDir, "APPS=catalog-rpc", "CATALOG_PORT="+port,
		"CATALOG_DEV_KEY="+taskDevKey, "CATALOG_CONFIG="+config)
	cmd.Env = taskEnv(env)
	r := &taskRun{port: port, out: &syncBuf{}, done: make(chan error, 1), cancel: cancel}
	cmd.Stdout, cmd.Stderr = r.out, r.out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { r.done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if !r.exited {
			<-r.done
		}
	})

	return r
}

// prebuild compiles catalog-rpc into binDir once, outside any case's deadline: the Taskfile's own
// build dependency then only finds the build cache warm.
func prebuild(t *testing.T, binDir string) {
	t.Helper()
	root, _ := filepath.Abs("../..")
	cmd, _ := boundedCommand(t, 4*time.Minute, root, "go", "build", "-o", filepath.Join(binDir, "catalog-rpc"), "./apps/catalog-rpc")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
}

// settle waits until the run serves /api/readyz 200 (true) or exits (false), up to 30s. A run that
// exited keeps its error in r.err.
func (r *taskRun) settle(t *testing.T) bool {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case r.err = <-r.done:
			r.exited = true

			return false
		case <-deadline:
			t.Fatalf("neither served nor exited in 30s:\n%s", r.out)
		case <-time.After(100 * time.Millisecond):
			res, err := http.Get("http://127.0.0.1:" + r.port + "/api/readyz")
			if err != nil {
				continue
			}
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return true
			}
		}
	}
}

// listProducts calls an authenticated method with key ("" = none) and returns the HTTP status.
func (r *taskRun) listProducts(t *testing.T, key string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+r.port+"/catalog.v1.CatalogService/ListProducts", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, res.Body)

	return res.StatusCode
}

// dedicatedFile writes the dedicated config: the example with log.format json (the sentinel that
// this file, not another, is the one loaded) and postgres.url = pg.
func dedicatedFile(t *testing.T, dir, name, pg string) string {
	t.Helper()
	raw, err := os.ReadFile(exampleConfig)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(raw), "format: text", "format: json", 1)
	s = strings.Replace(s, "postgres://app:app@127.0.0.1:5432/app?sslmode=disable", pg, 1)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

const servedAsJSON = `"msg":"server listening"`

// A shell that already exports APP_CONFIG / APP_HTTP_ADDR / APP_AUTH_* (an old session, a direnv)
// must not change what the task runs: not the file (configs/local.yaml has the OIDC settings
// catalog-rpc refuses), not the port, not the auth mode, not the keys.
func TestTaskEntry_PinnedSettingsIgnoreInheritedEnv(t *testing.T) {
	bin, dir := t.TempDir(), t.TempDir()
	prebuild(t, bin)
	pg := freshDB(t).URL
	local := filepath.Join(dir, "local.yaml") // stands in for configs/local.yaml
	if err := os.WriteFile(local, []byte("log:\n  format: text\nauth:\n  oidc:\n    enabled: true\n    issuers:\n      - issuer: https://issuer.example\n        audience: console\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, elsewhere, _ := net.SplitHostPort(freeAddr(t))
	const otherKey = "someone-elses-key-0123456789abcdef"

	for name, env := range map[string]map[string]string{
		"nothing inherited": {},
		"APP_CONFIG":        {"APP_CONFIG": local},
		"APP_HTTP_ADDR":     {"APP_HTTP_ADDR": ":" + elsewhere},
		"APP_AUTH_MODE":     {"APP_AUTH_MODE": "off"},
		"APP_AUTH_API_KEYS": {"APP_AUTH_API_KEYS": "other=" + otherKey + ":admin"},
	} {
		t.Run(name, func(t *testing.T) {
			r := startTask(t, bin, dedicatedFile(t, t.TempDir(), "catalog-rpc.yaml", pg), env)
			if !r.settle(t) {
				t.Fatalf("task catalog-rpc did not serve (%v):\n%s", r.err, r.out)
			}
			if !strings.Contains(r.out.String(), servedAsJSON) {
				t.Errorf("not running on the dedicated file (log.format json expected):\n%s", r.out)
			}
			if got := r.listProducts(t, ""); got != http.StatusUnauthorized {
				t.Errorf("no key: status %d, want 401 (auth.mode required)", got)
			}
			if got := r.listProducts(t, otherKey); got != http.StatusUnauthorized {
				t.Errorf("an inherited key: status %d, want 401", got)
			}
			if got := r.listProducts(t, taskDevKey); got != http.StatusOK {
				t.Errorf("the task's dev key: status %d, want 200", got)
			}
		})
	}
}

// Every other APP_* keeps env > file, and a non-empty CATALOG_DATABASE_URL beats both; an empty
// one is "not set".
func TestTaskEntry_PostgresPrecedence(t *testing.T) {
	bin := t.TempDir()
	prebuild(t, bin)
	good := freshDB(t).URL

	for name, c := range map[string]struct {
		file   string
		env    map[string]string
		served bool
	}{
		"file":                                        {good, nil, true},
		"dead file":                                   {deadPG, nil, false},
		"CATALOG_DATABASE_URL beats the file":         {deadPG, map[string]string{"CATALOG_DATABASE_URL": good}, true},
		"empty CATALOG_DATABASE_URL is not set":       {deadPG, map[string]string{"CATALOG_DATABASE_URL": ""}, false},
		"APP_POSTGRES_URL beats the file":             {good, map[string]string{"APP_POSTGRES_URL": deadPG}, false},
		"CATALOG_DATABASE_URL beats APP_POSTGRES_URL": {deadPG, map[string]string{"APP_POSTGRES_URL": deadPG, "CATALOG_DATABASE_URL": good}, true},
	} {
		t.Run(name, func(t *testing.T) {
			r := startTask(t, bin, dedicatedFile(t, t.TempDir(), "catalog-rpc.yaml", c.file), c.env)
			if got := r.settle(t); got != c.served {
				t.Fatalf("served = %v, want %v:\n%s", got, c.served, r.out)
			}
			if !c.served && (r.err == nil) {
				t.Errorf("a refused start must leave the task non-zero:\n%s", r.out)
			}
		})
	}
}

// CATALOG_CONFIG is one path: absolute, relative to the task's directory, with spaces. A missing
// file is seeded from the example (byte for byte), an existing one is never overwritten, and a
// cp that fails fails the task.
func TestTaskEntry_ConfigPath(t *testing.T) {
	bin := t.TempDir()
	prebuild(t, bin)
	good := map[string]string{"CATALOG_DATABASE_URL": freshDB(t).URL}
	example, err := os.ReadFile(exampleConfig)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := filepath.Abs("../..")

	for _, form := range []string{"absolute", "relative", "with space", `double quote`} {
		dir := t.TempDir()
		if form == "with space" {
			dir = filepath.Join(dir, "a dir", "with space")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		path := func(name string) string {
			if form == "double quote" { // a legal file name: every place that uses the path must keep it one word
				name = strings.Replace(name, ".yaml", `"q.yaml`, 1)
			}
			p := filepath.Join(dir, name)
			if form == "relative" {
				rel, err := filepath.Rel(root, p)
				if err != nil {
					t.Fatal(err)
				}

				return rel
			}

			return p
		}

		t.Run(form+"/missing is seeded", func(t *testing.T) {
			p := path("catalog-rpc.yaml")
			r := startTask(t, bin, p, good)
			if !r.settle(t) {
				t.Fatalf("did not serve (%v):\n%s", r.err, r.out)
			}
			abs := p
			if !filepath.IsAbs(p) {
				abs = filepath.Join(root, p)
			}
			if got, err := os.ReadFile(abs); err != nil || !bytes.Equal(got, example) {
				t.Errorf("seeded file differs from the example (err %v)", err)
			}
		})
		t.Run(form+"/existing is kept", func(t *testing.T) {
			sentinel := strings.Replace(string(example), "format: text", "format: json", 1) + "\n# sentinel: not mine to overwrite\n"
			p := path("kept.yaml")
			abs := p
			if !filepath.IsAbs(p) {
				abs = filepath.Join(root, p)
			}
			if err := os.WriteFile(abs, []byte(sentinel), 0o600); err != nil {
				t.Fatal(err)
			}
			r := startTask(t, bin, p, good)
			if !r.settle(t) {
				t.Fatalf("did not serve (%v):\n%s", r.err, r.out)
			}
			if got, _ := os.ReadFile(abs); string(got) != sentinel {
				t.Errorf("an existing file was overwritten:\n%s", got)
			}
			if !strings.Contains(r.out.String(), servedAsJSON) {
				t.Errorf("not running on that file:\n%s", r.out)
			}
		})
	}

	t.Run("empty file is kept and refused", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "catalog-rpc.yaml")
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		r := startTask(t, bin, p, nil) // no CATALOG_DATABASE_URL: postgres.url is empty, --require-durable refuses
		if r.settle(t) || r.err == nil {
			t.Fatalf("an empty file without a Postgres URL must be refused, non-zero:\n%s", r.out)
		}
		if st, err := os.Stat(p); err != nil || st.Size() != 0 {
			t.Errorf("the empty file was overwritten (err %v)", err)
		}
	})

	t.Run("a cp that fails fails the task", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "no", "such", "dir", "catalog-rpc.yaml")
		r := startTask(t, bin, p, good)
		if r.settle(t) || r.err == nil {
			t.Fatalf("seeding into a missing directory must fail the task:\n%s", r.out)
		}
		if _, err := os.Stat(p); err == nil {
			t.Error("the file exists")
		}
	})
}

// TestTaskEntry_HeldDB is the inner half of the timeout tests below: a task run against the
// Postgres in TASK_ENTRY_HELD_DB. It only runs when those tests drive it. Its own clock is no part
// of the scenario: it waits for the outer test to say catalog-rpc is stuck on that Postgres
// (the marker file), then cancels the context the deadline would cancel, and the whole group must go.
func TestTaskEntry_HeldDB(t *testing.T) {
	url, marker := os.Getenv("TASK_ENTRY_HELD_DB"), os.Getenv("TASK_ENTRY_STALL_MARKER")
	if url == "" {
		t.Skip("driven by TestTaskEntry_ParentTimeoutLeavesNoChildren")
	}
	r := startTask(t, os.Getenv("TASK_ENTRY_BIN"), dedicatedFile(t, t.TempDir(), "catalog-rpc.yaml", url), nil)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		select {
		case r.err = <-r.done:
			r.exited = true
			t.Fatalf("the run ended by itself (%v) before the stall was observed:\n%s", r.err, r.out)
		case <-time.After(50 * time.Millisecond):
		}
		if res, err := http.Get("http://127.0.0.1:" + r.port + "/api/readyz"); err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				t.Fatalf("catalog-rpc served: it was not stuck on its Postgres:\n%s", r.out)
			}
		}
	}
	r.cancel()
	select {
	case r.err = <-r.done:
		r.exited = true
	case <-time.After(15 * time.Second):
		t.Fatalf("the group was not killed 15s after the cancel:\n%s", r.out)
	}
	var ee *exec.ExitError
	if !errors.As(r.err, &ee) || !ee.Sys().(syscall.WaitStatus).Signaled() || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("the run did not end by the group kill (%v):\n%s", r.err, r.out)
	}
}

// psRows is pid -> (ppid, command) of every process.
func psRows(t *testing.T) map[int][2]string {
	t.Helper()
	out, err := exec.Command("ps", "-Ao", "pid=,ppid=,command=").Output()
	if err != nil {
		t.Fatal(err)
	}
	rows := map[int][2]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		if err1 != nil {
			continue
		}
		rows[pid] = [2]string{f[1], strings.Join(f[2:], " ")}
	}

	return rows
}

// heldRun is what one run of TestTaskEntry_HeldDB as a process of its own (with a short
// -test.timeout, so the go test alarm is what ends it) left behind.
type heldRun struct {
	seen     map[int]string // every descendant seen while it ran: pid -> command
	alive    []string       // those still alive 4s after it ended
	out      string         // its output
	innerErr error          // how the inner test process ended (nil: it passed)
	accepted int            // connections the never-answering Postgres accepted
	notified bool           // the inner test was told that catalog-rpc is stuck on it
}

// sawCatalog: the catalog-rpc binary itself (not task, not go build) is among the descendants seen.
func (h heldRun) sawCatalog() bool {
	for _, cmdline := range h.seen {
		if strings.HasSuffix(cmdline, "/catalog-rpc --require-durable") {
			return true
		}
	}

	return false
}

// enteredStall says whether the run really went through the scenario it claims to test, so that
// "nothing is left over" means the stuck catalog-rpc was cleaned up and not that it never existed:
// the inner test passed (it only does when the harness's deadline killed the stuck group), the
// catalog-rpc binary itself was seen running, and it connected to the Postgres that never answers.
func (h heldRun) enteredStall() error {
	if !h.notified {
		return errors.New("never reached the held DB: catalog-rpc did not connect to the Postgres that never answers")
	}
	if h.innerErr != nil {
		return fmt.Errorf("the inner test did not pass (%w)", h.innerErr)
	}
	if !h.sawCatalog() {
		return errors.New("the catalog-rpc binary was never seen: only task or go build ran")
	}
	if h.accepted == 0 {
		return errors.New("the Postgres that never answers got no connection: catalog-rpc was not stuck on it")
	}

	return nil
}

// runHeldDB runs TestTaskEntry_HeldDB as a process of its own, against the Postgres URL dbURL
// builds from the address of a listener that accepts connections and never answers.
func runHeldDB(t *testing.T, dbURL func(heldAddr string) string, slowBuild time.Duration) heldRun {
	t.Helper()
	bin := t.TempDir()
	prebuild(t, bin)
	path := os.Getenv("PATH")
	if slowBuild > 0 { // the inner task's own `go build` is slow: a legal, merely slow start
		goBin, err := exec.LookPath("go")
		if err != nil {
			t.Fatal(err)
		}
		wrap := t.TempDir()
		script := "#!/bin/sh\nif [ \"$1\" = build ]; then sleep " + strconv.Itoa(int(slowBuild.Seconds())) + "; fi\nexec " + goBin + " \"$@\"\n"
		if err := os.WriteFile(filepath.Join(wrap, "go"), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		path = wrap + string(os.PathListSeparator) + path
	}
	hold, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hold.Close() })
	var (
		heldMu sync.Mutex
		held   []net.Conn
	)
	accepted := 0
	t.Cleanup(func() {
		heldMu.Lock()
		defer heldMu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})
	go func() { // fire-and-forget: ends when the listener closes at cleanup
		for {
			c, err := hold.Accept()
			if err != nil {
				return
			}
			heldMu.Lock()
			held = append(held, c) // accepted and never answered
			accepted++
			heldMu.Unlock()
		}
	}()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "stalled")
	cmd := exec.Command(exe, "-test.run=^TestTaskEntry_HeldDB$", "-test.timeout=90s", "-test.v") // 90s: only a fail-safe
	cmd.Env = append(taskEnv(nil), "TASK_ENTRY_STALL_MARKER="+marker,
		"TASK_ENTRY_HELD_DB="+dbURL(hold.Addr().String()),
		"TASK_ENTRY_BIN="+bin, "TEST_DATABASE_URL="+pgtest.URL(t), "PATH="+path)
	var out syncBuf
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }() // fire-and-forget: reports into the buffered channel read below

	// Whatever descends from the inner test while it runs; remembered with its command, so a
	// recycled pid is not mistaken for a survivor.
	h := heldRun{seen: map[int]string{}}
	t.Cleanup(func() { // the leak must not outlive this test either
		for pid := range h.seen {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	track := func() {
		rows := psRows(t)
		family := map[int]bool{cmd.Process.Pid: true}
		for changed := true; changed; {
			changed = false
			for pid, row := range rows {
				if ppid, _ := strconv.Atoi(row[0]); family[ppid] && !family[pid] {
					family[pid], h.seen[pid], changed = true, row[1], true
				}
			}
		}
	}
	// The clock is the events: startup (task, its build, catalog-rpc connecting to the held
	// Postgres) gets up to 60s and then fails as "never reached the held DB"; once catalog-rpc is seen
	// and the Postgres has accepted its connection, the inner test is told to cancel.
	startup := time.After(60 * time.Second)
	for exited := false; !exited; {
		select {
		case h.innerErr = <-done:
			exited = true
		case <-startup:
			if !h.notified {
				_ = cmd.Process.Kill() // the run is reported as never having reached the held DB
			}
		case <-time.After(100 * time.Millisecond):
			track()
			heldMu.Lock()
			h.accepted = accepted
			heldMu.Unlock()
			if !h.notified && h.accepted > 0 && h.sawCatalog() {
				h.notified = true
				if err := os.WriteFile(marker, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	h.out = out.String()
	heldMu.Lock()
	h.accepted = accepted
	heldMu.Unlock()

	for deadline := time.Now().Add(4 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		h.alive = h.alive[:0]
		rows := psRows(t)
		for pid, cmdline := range h.seen {
			if row, ok := rows[pid]; ok && row[1] == cmdline {
				h.alive = append(h.alive, strconv.Itoa(pid)+" "+cmdline)
			}
		}
		if len(h.alive) == 0 || time.Now().After(deadline) {
			break
		}
	}

	return h
}

func heldURL(addr string) string { return "postgres://app:app@" + addr + "/app?sslmode=disable" }

// A legal but slow start (the inner task's go build takes 9s here) is the same scenario, only
// later: the stall is cleaned up all the same, and the run must not fail for being slow.
func TestTaskEntry_ParentTimeoutWithASlowBuild(t *testing.T) {
	h := runHeldDB(t, heldURL, 9*time.Second)
	if err := h.enteredStall(); err != nil {
		t.Errorf("the timeout scenario did not happen: %v:\n%s", err, h.out)
	}
	if len(h.alive) > 0 {
		t.Errorf("children outlived the parent test's timeout:\n  %s", strings.Join(h.alive, "\n  "))
	}
}

// The harness starts task, its shell and catalog-rpc in a process group of their own. When the
// test binary is killed by `go test -timeout` (a panic: no t.Cleanup runs) they must not outlive
// it: the children's budget is shorter than the parent's remaining deadline, and at the deadline
// the whole group is killed and waited for. The inner test runs against a Postgres that accepts
// connections and never answers, so catalog-rpc is stuck; what is left of that process tree
// afterwards is what the harness leaked.
func TestTaskEntry_ParentTimeoutLeavesNoChildren(t *testing.T) {
	h := runHeldDB(t, heldURL, 0)
	if err := h.enteredStall(); err != nil {
		t.Errorf("the timeout scenario did not happen: %v:\n%s", err, h.out)
	}
	if len(h.alive) > 0 {
		t.Errorf("children outlived the parent test's timeout:\n  %s\ninner output:\n%s", strings.Join(h.alive, "\n  "), h.out)
	}
}

// The cleanup assertion above only means something if the run really was stuck on the Postgres
// that never answers. A Postgres that refuses at once (catalog-rpc exits by itself, nothing is
// left to leak) must not pass for that scenario: the check must reject it.
func TestTaskEntry_ParentTimeoutScenarioRejectsAnEarlyEnd(t *testing.T) {
	h := runHeldDB(t, func(string) string { return deadPG }, 0)
	err := h.enteredStall()
	if err == nil {
		t.Fatalf("a Postgres that refuses at once passed as the stall scenario:\n%s", h.out)
	}
	t.Logf("rejected, as it must be: %v", err)
}
