package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// The server reads one request message of at most connect-go v2's default, 4 MiB; it is not a
// setting (#554). A message of exactly the limit is served, one byte more is refused with the
// library's resource_exhausted and no x-error-code (this repo adds none). The message text of that
// refusal is the library's and is deliberately not asserted.
func TestServer_ReadLimitIsTheConnectDefault(t *testing.T) {
	const limit = 4 << 20
	tel := &otel.Providers{
		ServiceName:    "readlimit-test",
		Tracer:         sdktrace.NewTracerProvider(),
		Meter:          noop.NewMeterProvider(),
		Propagator:     propagation.TraceContext{},
		MetricsHandler: http.NotFoundHandler(),
	}
	srv, err := server.New(config.HTTP{Addr: ":0"}, config.Auth{}, slog.New(slog.NewTextHandler(&logBuffer{}, nil)),
		[]server.Route{pingRoute{}}, nil, nil, tel, nil, server.OIDCAuth{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// A valid Empty request of exactly n bytes: "{", spaces, "}".
	post := func(n int) (int, string, string) {
		t.Helper()
		body := "{" + strings.Repeat(" ", n-2) + "}"
		res, err := http.Post(ts.URL+pingProcedure, "application/json", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		raw, _ := io.ReadAll(res.Body)
		var out struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(raw, &out)

		return res.StatusCode, out.Code, res.Header.Get("X-Error-Code")
	}

	if status, code, _ := post(limit); status != http.StatusOK {
		t.Errorf("a message of exactly %d bytes = %d %q, want 200", limit, status, code)
	}
	status, code, errCode := post(limit + 1)
	if code != "resource_exhausted" || errCode != "" {
		t.Errorf("a message of %d bytes = %d code %q x-error-code %q, want code resource_exhausted and no x-error-code", limit+1, status, code, errCode)
	}
	if status != http.StatusTooManyRequests {
		t.Errorf("a message of %d bytes = HTTP %d, want 429", limit+1, status)
	}
}
