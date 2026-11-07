//go:build unix

// Package reaptest is the harness that proves what happens to a throwaway test container when the test binary
// that started it ends in each of the ways a test binary can end (#481).
//
// The binary under test is the test binary of the package being checked: it re-runs ITSELF (os.Args[0],
// -test.run=^TestReapFixture$) as a fixture, so the real TestMain, the real pgtest/redistest and their real
// signal handling are what is exercised, not a copy. A package adds
//
//	func TestReapFixture(t *testing.T) { reaptest.Fixture(t, func(t *testing.T) string { return pgtest.URL(t) }) }
//	func TestReap(t *testing.T)        { reaptest.Scenarios(t) }
//
// The fixture's container is identified by the host port its URL names, never by an image or a label: the
// machine may be running other agents' tests (and other containers of the same image) at the same time, and a
// harness that counted "postgres containers" would count theirs. It is also why this harness needs nothing the
// reaper adds, so it can be written, and seen to fail, before the reaper exists.
//
// Every child is bounded (its own -test.timeout, a context shorter than the parent test's deadline), is waited
// for after any kill, and is killed by its own pid only (never by name). Everything it removes it identified itself.
package reaptest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/ctrreap"
)

// Environment of the fixture. The parent sets it, the fixture (this same test binary) reads it.
const (
	envMode      = "CTRREAP_FIXTURE" // "hold": stay up until killed / triggered; "once": start, report, exit
	envReady     = "CTRREAP_READY"   // file the fixture writes its URL to once its container is up
	envTrigger   = "CTRREAP_TRIGGER" // the fixture panics when this file exists
	envReport    = "CTRREAP_REPORT"  // file the fixture writes what its start's reap did to (ctrreap.LastReport), as JSON
	envNamespace = "X_TESTCTR_NAMESPACE"
)

// fixtureTest is the name of the test every adopting package defines; the child runs only that.
const fixtureTest = "TestReapFixture"

// Fixture is the body of TestReapFixture: it skips unless this binary was started by the harness, then asks start
// for the URL (which is what starts the container), tells the parent, and holds or returns according to the mode.
func Fixture(t *testing.T, start func(t *testing.T) string) {
	t.Helper()
	mode := os.Getenv(envMode)
	if mode == "" {
		t.Skip("a fixture of the reap harness; it only runs when the harness starts it")
	}
	u := start(t)
	if path := os.Getenv(envReport); path != "" {
		rep, _ := ctrreap.LastReport()
		b, err := json.Marshal(rep)
		if err != nil {
			t.Fatalf("reaptest: marshal report: %v", err)
		}
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatalf("reaptest: write report: %v", err)
		}
	}
	ready := os.Getenv(envReady)
	if ready == "" {
		t.Fatalf("reaptest: %s is not set", envReady)
	}
	if err := os.WriteFile(ready+".tmp", []byte(u), 0o600); err != nil {
		t.Fatalf("reaptest: write ready file: %v", err)
	}
	if err := os.Rename(ready+".tmp", ready); err != nil {
		t.Fatalf("reaptest: publish ready file: %v", err)
	}
	if mode == "once" {
		return
	}
	trigger := os.Getenv(envTrigger)
	for {
		if trigger != "" {
			if _, err := os.Stat(trigger); err == nil {
				panic("reaptest: the fixture was told to panic")
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Opts says how a child is started.
type Opts struct {
	Mode      string        // "hold" (default) or "once"
	Namespace string        // X_TESTCTR_NAMESPACE of the child; the harness's own, so no other process reaps or is reaped by it
	Timeout   time.Duration // the child's -test.timeout (default 4 minutes)
}

// Child is one fixture process.
type Child struct {
	t       *testing.T
	cmd     *exec.Cmd
	out     *syncBuffer
	done    chan struct{}
	waitErr error
	dir     string
	Ready   string // path of the ready file
	Trigger string // path of the trigger file
	report  string // path of the report file
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.b.Len() > 64<<10 { // a diagnostic tail, not an archive
		return len(p), nil
	}
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// NeedRuntime skips the test when no container runtime answers, outside CI; in CI the runtime is a given and its
// absence is a failure. Same rule as pgtest.URL.
func NeedRuntime(t *testing.T) client.APIClient {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	provider, err := testcontainers.NewDockerProvider()
	if err == nil {
		if _, err = provider.DaemonHost(c); err == nil {
			err = provider.Health(c)
		}
	}
	if err != nil {
		if os.Getenv("CI") == "" {
			t.Skipf("container runtime unreachable: %v", err)
		}
		t.Fatalf("container runtime unreachable in CI: %v", err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	return provider.Client()
}

// Spawn starts the fixture. The child ends with the test whatever happens: its context is cancelled before the
// parent's deadline, a cancel kills its process group, and Cleanup waits for it.
func Spawn(t *testing.T, o Opts) *Child {
	t.Helper()
	if o.Mode == "" {
		o.Mode = "hold"
	}
	if o.Timeout == 0 {
		o.Timeout = 4 * time.Minute
	}
	budget := o.Timeout + 30*time.Second
	if d, ok := t.Deadline(); ok {
		if left := time.Until(d) - 15*time.Second; left < budget {
			budget = left
		}
	}
	if budget <= 0 {
		t.Fatalf("reaptest: no time left in the parent test's deadline for a child")
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	dir := t.TempDir()
	ch := &Child{t: t, dir: dir, Ready: filepath.Join(dir, "ready"), Trigger: filepath.Join(dir, "trigger"), report: filepath.Join(dir, "report"), out: &syncBuffer{}, done: make(chan struct{})}

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+fixtureTest+"$", "-test.timeout="+o.Timeout.String(), "-test.v")
	cmd.Env = childEnv(map[string]string{envMode: o.Mode, envReady: ch.Ready, envTrigger: ch.Trigger, envReport: ch.report, envNamespace: o.Namespace})
	cmd.Stdout, cmd.Stderr = ch.out, ch.out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("reaptest: start fixture: %v", err)
	}
	ch.cmd = cmd
	go func() {
		ch.waitErr = cmd.Wait()
		close(ch.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-ch.done:
		case <-time.After(20 * time.Second):
			t.Errorf("reaptest: the fixture (pid %d) did not end after its context was cancelled", cmd.Process.Pid)
		}
	})
	return ch
}

// childEnv is this process's environment minus whatever would make the fixture use an existing database or redis
// instead of starting a container, plus the harness's settings.
func childEnv(set map[string]string) []string {
	drop := map[string]bool{"TEST_DATABASE_URL": true, "TEST_REDIS_URL": true, envMode: true, envReady: true, envTrigger: true, envReport: true, envNamespace: true}
	var env []string
	for _, kv := range os.Environ() {
		name := kv
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				name = kv[:i]
				break
			}
		}
		if !drop[name] {
			env = append(env, kv)
		}
	}
	for k, v := range set {
		if v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// Pid is the fixture's process id (its container's owner).
func (c *Child) Pid() int { return c.cmd.Process.Pid }

// WaitReady waits for the fixture's container to be up and returns the URL it hands out. A fixture that ends first,
// or does not get there in time, is a SETUP failure of the harness, said as such: nothing about the reaper can be
// concluded from a run that never had a container.
func (c *Child) WaitReady(budget time.Duration) string {
	c.t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(c.Ready); err == nil {
			return string(b)
		}
		select {
		case <-c.done:
			c.t.Fatalf("reaptest: SETUP FAILURE, not a result: the fixture ended (%v) before its container was up:\n%s", c.waitErr, c.out.String())
		case <-time.After(100 * time.Millisecond):
		}
	}
	c.t.Fatalf("reaptest: SETUP FAILURE, not a result: no container within %v:\n%s", budget, c.out.String())
	return ""
}

// Signal sends sig to the fixture's own pid.
func (c *Child) Signal(sig syscall.Signal) {
	c.t.Helper()
	if err := c.cmd.Process.Signal(sig); err != nil {
		c.t.Fatalf("reaptest: signal %v to pid %d: %v", sig, c.cmd.Process.Pid, err)
	}
}

// Kill SIGKILLs the fixture's own pid and waits for it to be gone.
func (c *Child) Kill() {
	c.t.Helper()
	_ = c.cmd.Process.Kill()
	c.Wait(20 * time.Second)
}

// Panic tells the fixture to panic and waits for it to be gone.
func (c *Child) Panic() {
	c.t.Helper()
	if err := os.WriteFile(c.Trigger, []byte("x"), 0o600); err != nil {
		c.t.Fatalf("reaptest: write trigger: %v", err)
	}
	c.Wait(30 * time.Second)
}

// Wait waits for the fixture to end and returns its exit code (-1 when a signal ended it). The process is reaped
// here, so by the time this returns it is not a zombie any more (a zombie still answers kill -0).
func (c *Child) Wait(budget time.Duration) int {
	c.t.Helper()
	select {
	case <-c.done:
	case <-time.After(budget):
		c.t.Fatalf("reaptest: the fixture (pid %d) did not end within %v:\n%s", c.cmd.Process.Pid, budget, c.out.String())
	}
	var ee *exec.ExitError
	if c.waitErr == nil {
		return 0
	}
	if errors.As(c.waitErr, &ee) {
		return ee.ExitCode()
	}
	c.t.Fatalf("reaptest: waiting for the fixture: %v", c.waitErr)
	return -1
}

// Output is what the fixture printed (its tail), for a failure message.
func (c *Child) Output() string { return c.out.String() }

// PortOf is the host port a container URL names.
func PortOf(t *testing.T, rawURL string) uint16 {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("reaptest: parse %q: %v", rawURL, err)
	}
	n, err := strconv.ParseUint(u.Port(), 10, 16)
	if err != nil || n == 0 {
		t.Fatalf("reaptest: no port in %q", rawURL)
	}
	return uint16(n)
}

// Containers is a window on the runtime for one test.
type Containers struct {
	t   *testing.T
	api client.APIClient
}

// On wraps a runtime client.
func On(t *testing.T, api client.APIClient) *Containers { return &Containers{t: t, api: api} }

// IDByPort finds the container publishing hostPort, or "" when there is none. It lists every container (running or
// not) and matches the port itself, so it does not depend on a filter the runtime may implement differently.
func (c *Containers) IDByPort(hostPort uint16) string {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := c.api.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		c.t.Fatalf("reaptest: list containers: %v", err)
	}
	for _, s := range res.Items {
		for _, p := range s.Ports {
			if p.PublicPort == hostPort {
				return s.ID
			}
		}
	}
	return ""
}

// Exists reports whether the container id is still there.
func (c *Containers) Exists(id string) bool {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := c.api.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		c.t.Fatalf("reaptest: list containers: %v", err)
	}
	for _, s := range res.Items {
		if s.ID == id {
			return true
		}
	}
	return false
}

// GoneWithin waits up to budget for the container to disappear and says whether it did.
func (c *Containers) GoneWithin(id string, budget time.Duration) bool {
	c.t.Helper()
	deadline := time.Now().Add(budget)
	for {
		if !c.Exists(id) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Remove force-removes a container this harness identified itself (cleanup for a container the code under test left
// behind). Never called with an id found any other way.
func (c *Containers) Remove(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.api.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
		c.t.Logf("reaptest: cleanup: remove %.12s: %v", id, err)
	}
}

// Namespace is a name no other process uses, so the fixtures of one test never touch another process's containers
// and no other process's reaper touches theirs.
func Namespace(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("reaptest-%d-%d", os.Getpid(), time.Now().UnixNano())
}

// Report is what the fixture's start did about orphans (written before it says it is ready). The fixture can only report
// a reap that ran: a missing or unreadable report is a setup failure.
func (c *Child) Report() ctrreap.Report {
	c.t.Helper()
	b, err := os.ReadFile(c.report)
	if err != nil {
		c.t.Fatalf("reaptest: SETUP FAILURE, not a result: no report from the fixture: %v\n%s", err, c.out.String())
	}
	var rep ctrreap.Report
	if err := json.Unmarshal(b, &rep); err != nil {
		c.t.Fatalf("reaptest: unreadable report %q: %v", b, err)
	}
	return rep
}

// Create starts a container of image that only sleeps, with exactly these labels, and removes it when the test ends.
// It stands in for a container some test process left (or is using): nothing about it comes from pgtest or redistest.
func (c *Containers) Create(image string, labels map[string]string) string {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	created, err := c.api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{Image: image, Entrypoint: []string{"sleep"}, Cmd: []string{"600"}, Labels: labels},
	})
	if err != nil {
		c.t.Fatalf("reaptest: SETUP FAILURE, not a result: create a synthetic container of %s: %v", image, err)
	}
	id := created.ID
	c.t.Cleanup(func() {
		if c.Exists(id) {
			c.Remove(id)
		}
	})
	if _, err := c.api.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		c.t.Fatalf("reaptest: SETUP FAILURE, not a result: start the synthetic container: %v", err)
	}
	return id
}

// Owner describes who a synthetic container claims to belong to.
type Owner = ctrreap.Owner

// DeadOwner is an owner that really existed on this host and is gone: a child that was started, had its start time
// read, and was killed and waited for. Its pid does not exist (or, if the system already reused it, has another start
// time: either way the owner is gone).
func DeadOwner(t *testing.T) Owner {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("reaptest: start the owner stand-in: %v", err)
	}
	pid := cmd.Process.Pid
	start, running, err := ctrreap.ProcessStart(pid)
	if err != nil || !running {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("reaptest: SETUP FAILURE, not a result: cannot read the start time of the stand-in (running=%v err=%v)", running, err)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	host, _ := os.Hostname()
	return Owner{Host: host, PID: pid, StartMS: start}
}

// SelfOwner is this test process: alive, with its real start time.
func SelfOwner(t *testing.T) Owner {
	t.Helper()
	o, err := ctrreap.Self()
	if err != nil {
		t.Fatalf("reaptest: SETUP FAILURE, not a result: %v", err)
	}
	return o
}

// MarkerLabels are the labels of a container of the namespace claiming owner o.
func MarkerLabels(namespace string, o Owner) map[string]string {
	return map[string]string{
		ctrreap.LabelNamespace: namespace,
		ctrreap.LabelHost:      o.Host,
		ctrreap.LabelPID:       strconv.Itoa(o.PID),
		ctrreap.LabelStart:     strconv.FormatInt(o.StartMS, 10),
	}
}

// OwnerLabels are the owner labels alone, without the marker.
func OwnerLabels(o Owner) map[string]string {
	l := MarkerLabels("", o)
	delete(l, ctrreap.LabelNamespace)
	return l
}
