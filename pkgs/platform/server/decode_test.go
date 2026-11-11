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

// connect-go v2 reads the request inside the handler, so a body that does not decode now runs the
// whole interceptor chain first. v1 decoded before the chain: a malformed body from an anonymous
// caller got 400 invalid_argument, no X-Request-Id, no access line and no rate-limit token.
// Accepted by Sean (#553, decision d-20261111053749428610); this test pins the new behaviour at the real
// entry, an HTTP request to server.New with auth required and a one-token rate limit.
func TestDecodeFailure_RunsInsideTheInterceptors(t *testing.T) {
	const key = "ops-key-0123456789abc"
	post := func(ts *httptest.Server, headers map[string]string, body string) (int, http.Header, map[string]any) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, ts.URL+pingProcedure, bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		raw, _ := io.ReadAll(res.Body)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)

		return res.StatusCode, res.Header, out
	}
	newServer := func(t *testing.T) (*httptest.Server, *logBuffer) {
		t.Helper()
		logs := &logBuffer{}
		tel := &otel.Providers{
			ServiceName:    "decode-test",
			Tracer:         sdktrace.NewTracerProvider(),
			Meter:          noop.NewMeterProvider(),
			Propagator:     propagation.TraceContext{},
			MetricsHandler: http.NotFoundHandler(),
		}
		srv, err := server.New(config.HTTP{Addr: ":0", RateLimit: config.RateLimit{RPS: 0.01, Burst: 1}},
			config.Auth{Mode: config.AuthRequired, APIKeys: "ops=" + key + ":admin"},
			slog.New(slog.NewTextHandler(logs, nil)), []server.Route{pingRoute{}}, nil, nil, tel, nil, server.OIDCAuth{})
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)

		return ts, logs
	}
	accessLine := func(logs *logBuffer) string {
		for _, l := range strings.Split(logs.String(), "\n") {
			if strings.Contains(l, "msg=rpc ") {
				return l
			}
		}

		return ""
	}
	good := map[string]string{"X-API-Key": key}

	for _, tt := range []struct {
		name        string
		headers     map[string]string
		wantStatus  int
		wantCode    string
		wantErrCode string // x-error-code of the answer ("" = none: a decode failure has none)
		wantLine    []string
	}{
		{"no credential, malformed body", nil, http.StatusUnauthorized, "unauthenticated", server.UnauthenticatedCode,
			[]string{"code=unauthenticated", "auth=anonymous"}},
		{"valid credential, malformed body", good, http.StatusBadRequest, "invalid_argument", "",
			[]string{"code=invalid_argument", "auth=api_key", "sub=ops"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ts, logs := newServer(t)
			status, header, body := post(ts, tt.headers, `{not json`)
			if status != tt.wantStatus || body["code"] != tt.wantCode || header.Get("X-Error-Code") != tt.wantErrCode {
				t.Fatalf("answer = %d code %v x-error-code %q, want %d %s %q", status, body["code"], header.Get("X-Error-Code"), tt.wantStatus, tt.wantCode, tt.wantErrCode)
			}
			if header.Get(server.RequestIDHeader) == "" {
				t.Fatalf("no %s on the answer: the meta interceptor did not run", server.RequestIDHeader)
			}
			line := accessLine(logs)
			if line == "" {
				t.Fatalf("no rpc access line:\n%s", logs.String())
			}
			for _, want := range tt.wantLine {
				if !strings.Contains(line, " "+want) {
					t.Errorf("access line lacks %q: %s", want, line)
				}
			}
			// The malformed request took the only token: a good one right after is rate limited.
			status, header, _ = post(ts, good, `{}`)
			if status != http.StatusTooManyRequests || header.Get("X-Error-Code") != server.RateLimitedCode {
				t.Fatalf("good request after the malformed one = %d x-error-code %q, want 429 %s (the malformed body must spend the token)", status, header.Get("X-Error-Code"), server.RateLimitedCode)
			}
		})
	}

	// Control: with no malformed request first, the same good request passes, so the 429 above is
	// the malformed request's doing and not an empty bucket.
	t.Run("control: good request on a fresh server", func(t *testing.T) {
		ts, logs := newServer(t)
		if status, _, _ := post(ts, good, `{}`); status != http.StatusOK {
			t.Fatalf("good request = %d, want 200", status)
		}
		if line := accessLine(logs); !strings.Contains(line, " code=ok") {
			t.Fatalf("access line = %q, want code=ok", line)
		}
	})
}
