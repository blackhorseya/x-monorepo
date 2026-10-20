package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
)

// slowRoute is a Route whose handler blocks until released: an in-flight
// request at shutdown time.
type slowRoute struct {
	started chan struct{} // one send per request reaching the handler
	release chan struct{} // close to let every blocked request finish
}

func (r *slowRoute) Route(...connect.HandlerOption) (string, http.Handler) {
	return "/test.v1.Slow/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		r.started <- struct{}{}
		<-r.release
		_, _ = io.WriteString(w, "done")
	})
}

// newTestServer builds a Server on a 127.0.0.1:0 listener and runs Serve on
// it. It returns the base URL, the cancel that starts the shutdown and the
// channel Serve's result lands on.
func newTestServer(t *testing.T, cfg config.HTTP, route Route) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	tel, cleanup, err := otel.New(config.OTel{}, config.Log{Level: "error", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, config.Auth{}, logger, []Route{route}, nil, nil, tel)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(c, ln) }()

	return "http://" + ln.Addr().String(), cancel, done
}

func get(t *testing.T, client *http.Client, url string) (int, string, error) {
	t.Helper()
	res, err := client.Get(url)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)

	return res.StatusCode, string(body), nil
}

// The shutdown sequence over a real socket, with one in-flight request per
// protocol (HTTP/1.1 and h2c, what gRPC uses): readiness flips to 503 while
// the server still accepts traffic for shutdown_delay, then the listener
// closes, the in-flight requests complete normally and only then Serve returns.
func TestServer_Serve_GracefulShutdown(t *testing.T) {
	const delay = 300 * time.Millisecond
	route := &slowRoute{started: make(chan struct{}, 2), release: make(chan struct{})}
	base, cancel, done := newTestServer(t, config.HTTP{ShutdownDelay: delay, ShutdownTimeout: 5 * time.Second}, route)

	h2c := new(http.Protocols)
	h2c.SetUnencryptedHTTP2(true)
	clients := map[string]*http.Client{
		"http1": {Transport: &http.Transport{}},
		"h2c":   {Transport: &http.Transport{Protocols: h2c}},
	}
	if code, _, err := get(t, clients["http1"], base+ReadyzPath); err != nil || code != 200 {
		t.Fatalf("ready before shutdown = %d, %v", code, err)
	}

	// One request per protocol, blocked inside the handler.
	var wg sync.WaitGroup
	results := make(map[string]string, len(clients))
	var mu sync.Mutex
	for name, client := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, body, err := get(t, client, base+"/test.v1.Slow/Call")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				results[name] = "error: " + err.Error()
				return
			}
			results[name] = body + "/" + http.StatusText(code)
		}()
	}
	for range clients {
		select {
		case <-route.started:
		case <-time.After(5 * time.Second):
			t.Fatal("in-flight requests never reached the handler")
		}
	}

	shutdownAt := time.Now()
	cancel()

	// Inside the delay window: not ready, still alive, still serving.
	code, body, err := get(t, clients["http1"], base+ReadyzPath)
	if err != nil || code != http.StatusServiceUnavailable || !strings.Contains(body, `"server":"draining"`) {
		t.Fatalf("ready during drain = %d %q, %v", code, body, err)
	}
	if code, _, err := get(t, clients["http1"], base+HealthzPath); err != nil || code != 200 {
		t.Fatalf("live during drain = %d, %v", code, err)
	}
	select {
	case err := <-done:
		t.Fatalf("Serve returned (%v) with requests in flight", err)
	default:
	}

	// After the delay the listener is gone: a new connection is refused.
	for {
		_, _, err := get(t, &http.Client{Transport: &http.Transport{}}, base+HealthzPath)
		if err != nil {
			if time.Since(shutdownAt) < delay {
				t.Fatalf("listener closed after %s, before shutdown_delay %s", time.Since(shutdownAt), delay)
			}
			break
		}
		if time.Since(shutdownAt) > delay+5*time.Second {
			t.Fatal("listener still accepting long after shutdown_delay")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("Serve returned (%v) before the in-flight requests finished", err)
	case <-time.After(100 * time.Millisecond):
	}

	// Let the in-flight requests finish: both complete normally, then Serve returns clean.
	close(route.release)
	wg.Wait()
	for name, got := range results {
		if got != "done/OK" {
			t.Errorf("%s in-flight request during shutdown = %q, want done/OK", name, got)
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve = %v, want nil after a clean drain", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the in-flight requests finished")
	}
}

// A request that outlives shutdown_timeout is cut off and Serve reports it,
// so a pod's logs say why it did not drain cleanly.
func TestServer_Serve_ShutdownTimeout(t *testing.T) {
	route := &slowRoute{started: make(chan struct{}, 1), release: make(chan struct{})}
	defer close(route.release)
	base, cancel, done := newTestServer(t, config.HTTP{ShutdownTimeout: 200 * time.Millisecond}, route)

	go func() { _, _, _ = get(t, &http.Client{Transport: &http.Transport{}}, base+"/test.v1.Slow/Call") }()
	select {
	case <-route.started:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the handler")
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "shutdown_timeout") {
			t.Fatalf("Serve = %v, want shutdown_timeout deadline exceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not give up after shutdown_timeout")
	}
}
