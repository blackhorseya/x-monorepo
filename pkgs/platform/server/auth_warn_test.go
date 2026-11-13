package server_test

import (
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// The "relies solely on the credential resolver" Warn is true only when the
// CredentialResolver is the one wired credential source: no api_keys, no jwt,
// no OIDC. Each case also asserts that the same logger received "auth enabled",
// so an absent Warn means the condition said no, not that the logger was unplugged.
func TestNew_ResolverOnlyWarn(t *testing.T) {
	const warn = "auth relies solely on the credential resolver"
	oidcOn := server.OIDCAuth{Verifier: &fakeVerifier{}, Resolver: &fakeExternal{}}
	tests := []struct {
		name     string
		auth     config.Auth
		resolver server.CredentialResolver
		oidc     server.OIDCAuth
		wantWarn bool
	}{
		{"credential resolver only", config.Auth{Mode: config.AuthRequired}, &resolver{}, server.OIDCAuth{}, true},
		{"oidc only", config.Auth{Mode: config.AuthRequired}, nil, oidcOn, false},
		{"credential resolver and oidc", config.Auth{Mode: config.AuthRequired}, &resolver{}, oidcOn, false},
		{"api_keys", config.Auth{Mode: config.AuthRequired, APIKeys: "ops=0123456789abcdef0123456789abcdef"}, nil, server.OIDCAuth{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := &logBuffer{}
			tel := &otel.Providers{
				ServiceName: "auth-warn-test",
				Tracer:      sdktrace.NewTracerProvider(),
				Meter:       noop.NewMeterProvider(),
				Propagator:  propagation.TraceContext{},
			}
			if _, err := server.New(config.HTTP{Addr: ":0"}, tt.auth, slog.New(slog.NewTextHandler(logs, nil)),
				nil, nil, nil, tel, tt.resolver, tt.oidc); err != nil {
				t.Fatal(err)
			}
			out := logs.String()
			if !strings.Contains(out, "auth enabled") {
				t.Fatalf("the logger did not receive the \"auth enabled\" line, so the Warn check proves nothing: %q", out)
			}
			if got := strings.Contains(out, warn); got != tt.wantWarn {
				t.Fatalf("Warn %q present = %v, want %v; log: %q", warn, got, tt.wantWarn, out)
			}
		})
	}
}
