package server_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// The rpc access line and the span carry who called, for every outcome: the
// access line is written by the logging interceptor outside auth, so auth
// reports back through the context slot rather than a child context.
func TestAccessLine_Principal(t *testing.T) {
	cfg := config.Auth{
		Mode:    config.AuthOptional,
		APIKeys: "console=console-key-0123456789, ops=ops-key-0123456789abc:admin|ops",
		JWT:     config.JWT{HS256Secret: secret},
		Public:  []string{"/a.v1.S/Public"},
	}
	authn, err := server.NewAuthInterceptor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	valid := token(t, []byte(secret), jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "u-1", "name": "Ann", "roles": []string{"admin"}, "exp": time.Now().Add(time.Minute).Unix(),
	})
	chain := func(t *testing.T, withAuth bool) (func(proc string, headers map[string]string) error, *logBuffer, *tracetest.InMemoryExporter) {
		t.Helper()
		logs := &logBuffer{}
		logger := slog.New(slog.NewTextHandler(logs, nil))
		exp := tracetest.NewInMemoryExporter()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
		interceptors := []connect.Interceptor{server.NewMetaInterceptor(), server.NewLoggingInterceptor(logger)}
		if withAuth {
			interceptors = append(interceptors, authn)
		}
		var next connect.UnaryFunc = func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
			return connect.NewResponse(&struct{}{}), nil
		}
		for i := len(interceptors) - 1; i >= 0; i-- {
			next = interceptors[i].WrapUnary(next)
		}
		return func(proc string, headers map[string]string) error {
			req := connect.NewRequest(&struct{}{})
			for k, v := range headers {
				req.Header().Set(k, v)
			}
			c, span := tp.Tracer("test").Start(context.Background(), proc)
			defer span.End()
			_, err := next(c, request{AnyRequest: req, procedure: proc})
			return err
		}, logs, exp
	}

	tests := []struct {
		name      string
		withAuth  bool
		proc      string
		headers   map[string]string
		wantLine  []string // every fragment must be on the rpc line
		wantNot   []string // none may be
		wantSpan  map[attribute.Key]string
		wantRoles []string
	}{
		{"api key", true, "/a.v1.S/Get", map[string]string{server.APIKeyHeader: "console-key-0123456789"},
			[]string{"code=ok", "auth=api_key", "sub=console"}, nil, map[attribute.Key]string{"enduser.id": "console", "auth.method": "api_key"}, nil},
		{"api key with roles", true, "/a.v1.S/Get", map[string]string{server.APIKeyHeader: "ops-key-0123456789abc"},
			[]string{"auth=api_key", "sub=ops"}, nil, map[attribute.Key]string{"enduser.id": "ops"}, []string{"admin", "ops"}},
		{"jwt", true, "/a.v1.S/Get", map[string]string{server.AuthorizationHeader: "Bearer " + valid},
			[]string{"code=ok", "auth=jwt", "sub=u-1"}, []string{"Ann", "name="}, map[attribute.Key]string{"enduser.id": "u-1", "auth.method": "jwt"}, []string{"admin"}},
		{"anonymous (optional, no credential)", true, "/a.v1.S/Get", nil,
			[]string{"code=ok", "auth=anonymous"}, []string{"sub="}, map[attribute.Key]string{"auth.method": "anonymous"}, nil},
		{"rejected credential", true, "/a.v1.S/Get", map[string]string{server.APIKeyHeader: "nope-nope-nope-nope-nope"},
			[]string{"code=unauthenticated", "auth=anonymous"}, []string{"sub=", "nope-nope"}, map[attribute.Key]string{}, nil},
		{"public procedure", true, "/a.v1.S/Public", map[string]string{server.APIKeyHeader: "console-key-0123456789"},
			[]string{"code=ok", "auth=public"}, []string{"sub="}, map[attribute.Key]string{"auth.method": "public"}, nil},
		{"auth off (no interceptor)", false, "/a.v1.S/Get", map[string]string{server.APIKeyHeader: "console-key-0123456789"},
			[]string{"code=ok", "auth=anonymous"}, []string{"sub="}, map[attribute.Key]string{}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			call, logs, exp := chain(t, tt.withAuth)
			_ = call(tt.proc, tt.headers)

			var line string
			for _, l := range strings.Split(logs.String(), "\n") {
				if strings.Contains(l, "msg=rpc ") {
					line = l
				}
			}
			if line == "" {
				t.Fatalf("no rpc access line:\n%s", logs.String())
			}
			for _, want := range tt.wantLine {
				if !strings.Contains(line, " "+want) {
					t.Fatalf("rpc line %q lacks %q", line, want)
				}
			}
			for _, not := range tt.wantNot {
				if strings.Contains(line, not) {
					t.Fatalf("rpc line %q must not contain %q", line, not)
				}
			}

			spans := exp.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("spans = %d", len(spans))
			}
			attrs := map[attribute.Key]attribute.Value{}
			for _, kv := range spans[0].Attributes {
				attrs[kv.Key] = kv.Value
			}
			for k, want := range tt.wantSpan {
				if got := attrs[k].AsString(); got != want {
					t.Fatalf("span %s = %q, want %q (attrs %v)", k, got, want, spans[0].Attributes)
				}
			}
			if len(tt.wantSpan) == 0 {
				if _, ok := attrs["enduser.id"]; ok {
					t.Fatalf("span must not name a principal: %v", spans[0].Attributes)
				}
			}
			if got := attrs["enduser.roles"].AsStringSlice(); strings.Join(got, ",") != strings.Join(tt.wantRoles, ",") {
				t.Fatalf("span enduser.roles = %v, want %v", got, tt.wantRoles)
			}
		})
	}
}
