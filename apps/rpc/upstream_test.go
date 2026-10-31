package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// catalog-rpc is a separate process: the tests build the real binary once and run it per test, so the
// Connect client, the service key and the failure matrix are exercised across a real socket.
var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// childBudget is how long a child process may live: the budget asked for, but never past the test's own
// deadline (go test -timeout) less a margin, so the child is killed and reaped by its context BEFORE the
// parent's alarm panics the binary and strands it. A parent without a deadline gives the child the whole budget.
func childBudget(t *testing.T, want, margin time.Duration) time.Duration {
	t.Helper()
	d, ok := t.Deadline()

	return clampBudget(time.Now(), d, ok, want, margin)
}

// clampBudget is childBudget's arithmetic with the clock as arguments (0 means the parent has no time left).
func clampBudget(now, deadline time.Time, hasDeadline bool, want, margin time.Duration) time.Duration {
	if !hasDeadline {
		return want
	}
	left := max(deadline.Sub(now)-margin, 0)

	return min(want, left)
}

// childMargin is how long before the test binary's own deadline a child is gone: time to be killed and waited for.
const childMargin = 5 * time.Second

func catalogBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "catalog-rpc-bin")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "catalog-rpc")
		budget := childBudget(t, 3*time.Minute, childMargin)
		if budget <= 0 {
			buildErr = errors.New("the test binary is about to hit its own -timeout: no time left to build catalog-rpc")
			return
		}
		c, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()
		out, err := exec.CommandContext(c, "go", "build", "-o", binPath, "github.com/blackhorseya/x-monorepo/apps/catalog-rpc").CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("build catalog-rpc: %w\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}

	return binPath
}

// serviceKey is the key rpc presents and catalog-rpc accepts (rpc=<key>:admin).
const serviceKey = "rpc-service-key-0123456789abcd"

// catalogProc is one catalog-rpc process on a fixed address, so it can be stopped and brought back.
type catalogProc struct {
	t *testing.T
	// deadline bounds the child's life: shorter than the test binary's own budget, so a child that
	// outlives its test is killed and reaped by the context, not left to the parent's exit.
	deadline time.Duration
	cancel   context.CancelFunc
	URL      string
	addr     string
	env      []string
	cmd      *exec.Cmd
	done     chan struct{}
	logs     *lockedBuffer
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// startCatalogProc runs catalog-rpc with auth required and the single key rpc=serviceKey:admin; with no
// APP_POSTGRES_URL in extra it keeps the catalog in memory.
func startCatalogProc(t *testing.T, extra ...string) *catalogProc {
	t.Helper()

	return startCatalogProcWith(t, 90*time.Second, extra...)
}

func startCatalogProcWith(t *testing.T, deadline time.Duration, extra ...string) *catalogProc {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	p := &catalogProc{t: t, addr: addr, URL: "http://" + addr, logs: &lockedBuffer{}, deadline: deadline}
	p.env = append([]string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"),
		"APP_HTTP_ADDR=" + addr, "APP_LOG_LEVEL=info", "APP_LOG_FORMAT=json", "APP_HTTP_SHUTDOWN_DELAY=0",
		"APP_AUTH_MODE=required", "APP_AUTH_API_KEYS=rpc=" + serviceKey + ":admin",
	}, extra...)
	p.start()
	t.Cleanup(p.Stop)

	return p
}

func (p *catalogProc) start() {
	p.t.Helper()
	bin := catalogBinary(p.t) // built (once) before the child's clock starts: the build is not the child's time
	budget := childBudget(p.t, p.deadline, childMargin)
	if budget <= 0 {
		p.t.Fatalf("the test binary is about to hit its own -timeout: no time left to start catalog-rpc")
	}
	var ctx context.Context
	ctx, p.cancel = context.WithTimeout(context.Background(), budget)
	p.cmd = exec.CommandContext(ctx, bin)
	p.cmd.WaitDelay = 2 * time.Second
	p.cmd.Env = p.env
	p.cmd.Stdout, p.cmd.Stderr = p.logs, p.logs
	if err := p.cmd.Start(); err != nil {
		p.t.Fatal(err)
	}
	p.done = make(chan struct{})
	go func(cmd *exec.Cmd, done chan struct{}) { _ = cmd.Wait(); close(done) }(p.cmd, p.done)

	deadline := time.Now().Add(15 * time.Second)
	for {
		res, err := http.Get(p.URL + server.ReadyzPath)
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-p.done:
			p.t.Fatalf("catalog-rpc exited while starting:\n%s", p.logs.String())
		default:
		}
		if time.Now().After(deadline) {
			p.Stop()
			p.t.Fatalf("catalog-rpc never became ready:\n%s", p.logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Stop kills the process and waits for it: an outage, not a graceful drain. Safe to call twice.
func (p *catalogProc) Stop() {
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGKILL)
	p.cancel()
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		p.t.Errorf("catalog-rpc did not exit after SIGKILL")
	}
	p.cmd = nil
}

// Restart brings it back on the same address (a Postgres-backed catalog keeps its products).
func (p *catalogProc) Restart() {
	p.t.Helper()
	p.Stop()
	p.start()
}

// withCatalog points rpc at a catalog-rpc process; the key is rpc's service key.
func withCatalog(cfg *config.AppConfig, p *catalogProc) *config.AppConfig {
	cfg.Catalog = config.Catalog{Upstream: p.URL, APIKey: serviceKey, Timeout: 3 * time.Second}

	return cfg
}

// initialize is Initialize for tests: unless the test brought its own catalog, it runs a fresh
// in-memory catalog-rpc and points rpc at it.
func initialize(t *testing.T, cfg *config.AppConfig) (*server.Server, func(), error) {
	t.Helper()
	if cfg.Catalog.Upstream == "" && cfg.Catalog.APIKey == "" {
		withCatalog(cfg, startCatalogProc(t))
	}

	return Initialize(cfg)
}
