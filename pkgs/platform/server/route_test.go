package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
)

// WithHTTPRoute mounts a plain handler at one method and path. In auth.mode required it is NOT behind the
// API key (the interceptor wraps Connect procedures only), which is the point and the warning of the option.
func TestWithHTTPRoute(t *testing.T) {
	tel, cleanup, err := otel.New(config.OTel{}, config.Log{Level: "error", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	hits := 0
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		b, _ := io.ReadAll(r.Body)
		_, _ = io.WriteString(w, "got "+string(b))
	})
	srv, err := New(config.HTTP{Addr: ":0"}, config.Auth{Mode: config.AuthRequired, APIKeys: "rpc=0123456789abcdef0123456789abcdef"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil, tel, nil, OIDCAuth{}, WithHTTPRoute(http.MethodPost, "/api/hook", h))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(c, ln) }()
	base := "http://" + ln.Addr().String()

	res, err := http.Post(base+"/api/hook", "application/octet-stream", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || string(body) != "got payload" || hits != 1 {
		t.Fatalf("POST = %d %q hits %d; want 200 \"got payload\" 1 (no API key sent, auth required)", res.StatusCode, body, hits)
	}

	// only the method asked for
	res, err = http.Get(base + "/api/hook")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode == http.StatusOK || hits != 1 {
		t.Fatalf("GET = %d hits %d; the route answers POST only", res.StatusCode, hits)
	}
}
