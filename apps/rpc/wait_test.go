package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

// rpc --wait-catalog is what an initContainer runs before the rpc starts: it waits, with a bound, for
// catalog-rpc's readyz, so that on a rollout the catalog is normally up before the rpc takes traffic.
// It is only a best effort: when the catalog does not come, it logs a WARN and returns, so a catalog
// outage never keeps a new rpc pod from starting (rpc's own readyz does not depend on the catalog).
func TestParseArgs(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want cliOptions
		bad  bool
	}{
		"no args":                 {nil, cliOptions{}, false},
		"--version":               {[]string{"--version"}, cliOptions{version: true}, false},
		"-v":                      {[]string{"-v"}, cliOptions{version: true}, false},
		"--wait-catalog":          {[]string{"--wait-catalog"}, cliOptions{waitCatalog: true, waitBudget: 60 * time.Second}, false},
		"--wait-catalog=90s":      {[]string{"--wait-catalog=90s"}, cliOptions{waitCatalog: true, waitBudget: 90 * time.Second}, false},
		"a zero budget is a typo": {[]string{"--wait-catalog=0s"}, cliOptions{}, true},
		"a negative budget":       {[]string{"--wait-catalog=-5s"}, cliOptions{}, true},
		"not a duration":          {[]string{"--wait-catalog=soon"}, cliOptions{}, true},
		"an unknown flag":         {[]string{"--wait-catalogue"}, cliOptions{}, true},
		"flag and a stray arg":    {[]string{"--wait-catalog", "extra"}, cliOptions{}, true},
		"--version and the other": {[]string{"--version", "--wait-catalog"}, cliOptions{}, true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parseArgs(tc.args)
			if (err != nil) != tc.bad {
				t.Fatalf("parseArgs(%v) error = %v, want error: %v", tc.args, err, tc.bad)
			}
			if err == nil && got != tc.want {
				t.Fatalf("parseArgs(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

func readyzServer(t *testing.T, readyAfter time.Duration) (url string, calls *atomic.Int32) {
	t.Helper()
	calls = &atomic.Int32{}
	start := time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/readyz", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("X-API-Key") != "" || r.Header.Get("Authorization") != "" {
			t.Errorf("the wait must not present a credential")
		}
		if time.Since(start) < readyAfter {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	return "http://" + ln.Addr().String(), calls
}

func quietLogger(buf *bytes.Buffer) *slog.Logger { return slog.New(slog.NewTextHandler(buf, nil)) }

func TestWaitForCatalog_ReturnsAsSoonAsTheCatalogIsReady(t *testing.T) {
	url, calls := readyzServer(t, 0)
	var logs bytes.Buffer
	start := time.Now()
	if err := waitForCatalog(context.Background(), config.Catalog{Upstream: url, APIKey: "k-0123456789abcdef"}, 10*time.Second, 50*time.Millisecond, quietLogger(&logs)); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second || calls.Load() != 1 {
		t.Fatalf("a ready catalog: took %s with %d probes, want one probe and an immediate return", d, calls.Load())
	}
	if logs.Len() != 0 {
		t.Fatalf("a ready catalog logged: %s", logs.String())
	}
}

func TestWaitForCatalog_WaitsForACatalogThatIsComingUp(t *testing.T) {
	url, calls := readyzServer(t, 400*time.Millisecond)
	start := time.Now()
	if err := waitForCatalog(context.Background(), config.Catalog{Upstream: url, APIKey: "k-0123456789abcdef"}, 10*time.Second, 50*time.Millisecond, quietLogger(&bytes.Buffer{})); err != nil {
		t.Fatal(err)
	}
	d := time.Since(start)
	if d < 350*time.Millisecond || d > 3*time.Second || calls.Load() < 2 {
		t.Fatalf("took %s with %d probes, want a return soon after the catalog became ready (~400ms)", d, calls.Load())
	}
}

// Best effort: a catalog that never comes is a WARN and a normal return after the budget, not a failure.
func TestWaitForCatalog_GivesUpAfterTheBudgetWithAWarning(t *testing.T) {
	url, _ := readyzServer(t, time.Hour)
	var logs bytes.Buffer
	const key = "super-secret-service-key-0123456789"
	start := time.Now()
	if err := waitForCatalog(context.Background(), config.Catalog{Upstream: url, APIKey: key}, 500*time.Millisecond, 50*time.Millisecond, quietLogger(&logs)); err != nil {
		t.Fatalf("giving up after the budget is not an error: %v", err)
	}
	if d := time.Since(start); d < 450*time.Millisecond || d > 3*time.Second {
		t.Fatalf("took %s, want about the 500ms budget", d)
	}
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, url) {
		t.Fatalf("log = %q, want a WARN naming the upstream", out)
	}
	if strings.Contains(out, key) {
		t.Fatalf("the log carries the service key: %q", out)
	}
}

// A catalog nothing listens on is the same story, and the bound holds even when each probe fails at once.
func TestWaitForCatalog_NothingListening(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	_ = ln.Close()

	var logs bytes.Buffer
	start := time.Now()
	if err := waitForCatalog(context.Background(), config.Catalog{Upstream: dead, APIKey: "k-0123456789abcdef"}, 400*time.Millisecond, 50*time.Millisecond, quietLogger(&logs)); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 350*time.Millisecond || d > 3*time.Second {
		t.Fatalf("took %s, want about the 400ms budget", d)
	}
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("log = %q, want a WARN", logs.String())
	}
}

// SIGTERM during the wait ends it at once: the pod being deleted is not held by a probe loop.
func TestWaitForCatalog_StopsWhenTheContextEnds(t *testing.T) {
	url, _ := readyzServer(t, time.Hour)
	c, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start := time.Now()
	_ = waitForCatalog(c, config.Catalog{Upstream: url, APIKey: "k-0123456789abcdef"}, time.Minute, 50*time.Millisecond, quietLogger(&bytes.Buffer{}))
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %s after the context ended", d)
	}
}

// No upstream to wait for is a misconfiguration the server would refuse to start on too: that is an
// error (the initContainer fails and says so), unlike a catalog that is merely not up yet.
func TestWaitForCatalog_AMissingUpstreamIsAnError(t *testing.T) {
	for name, up := range map[string]string{"empty": "", "not a url": "catalog-rpc", "wrong scheme": "ftp://catalog-rpc"} {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			err := waitForCatalog(context.Background(), config.Catalog{Upstream: up}, time.Minute, 50*time.Millisecond, quietLogger(&bytes.Buffer{}))
			if err == nil || time.Since(start) > time.Second {
				t.Fatalf("err = %v after %s, want an immediate error", err, time.Since(start))
			}
		})
	}
}
