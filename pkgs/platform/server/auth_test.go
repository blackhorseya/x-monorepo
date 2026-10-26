package server_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

const secret = "0123456789abcdef0123456789abcdef" // 32 bytes

// call runs the interceptor on one fake unary request and returns the
// principal the next function saw, and the error.
func call(t *testing.T, cfg config.Auth, procedure string, headers map[string]string) (contextx.Principal, error) {
	t.Helper()
	in, err := server.NewAuthInterceptor(cfg, noop.NewMeterProvider(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if in == nil {
		t.Fatal("mode on but no interceptor")
	}
	var seen contextx.Principal
	next := in(func(c context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		seen = contextx.PrincipalFrom(c)
		return connect.NewResponse(&struct{}{}), nil
	})
	req := connect.NewRequest(&struct{}{})
	for k, v := range headers {
		req.Header().Set(k, v)
	}
	// Spec().Procedure comes from the handler in a real server; a request
	// built by hand gets it through the interceptor's view of the spec.
	_, err = next(context.Background(), request{AnyRequest: req, procedure: procedure})

	return seen, err
}

type request struct {
	connect.AnyRequest
	procedure string
}

func (r request) Spec() connect.Spec { return connect.Spec{Procedure: r.procedure} }

func token(t *testing.T, key any, method jwt.SigningMethod, cl jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(method, cl).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func errCode(err error) (connect.Code, string) {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return 0, ""
	}
	return ce.Code(), ce.Meta().Get("x-error-code")
}

func TestAuthInterceptor_Off(t *testing.T) {
	for _, mode := range []string{"", config.AuthOff} {
		in, err := server.NewAuthInterceptor(config.Auth{Mode: mode}, noop.NewMeterProvider(), nil)
		if err != nil || in != nil {
			t.Fatalf("mode %q: %v, %v", mode, in, err)
		}
	}
	if _, err := server.NewAuthInterceptor(config.Auth{Mode: config.AuthRequired}, noop.NewMeterProvider(), nil); err == nil {
		t.Fatal("required with nothing configured must fail")
	}
	if _, err := server.NewAuthInterceptor(config.Auth{Mode: config.AuthRequired, APIKeys: "ci=short"}, noop.NewMeterProvider(), nil); err == nil {
		t.Fatal("short api key must fail")
	}
	if _, err := server.NewAuthInterceptor(config.Auth{Mode: config.AuthRequired, JWT: config.JWT{HS256Secret: "short"}}, noop.NewMeterProvider(), nil); err == nil {
		t.Fatal("short hs256 secret must fail")
	}
}

func TestAuthInterceptor_Modes(t *testing.T) {
	keys := "ci=ci-key-0123456789abcdef:admin|ops, console = console-key-0123456789"
	now := time.Now()
	valid := token(t, []byte(secret), jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "u-1", "name": "Ann", "roles": []string{"admin"}, "iss": "x", "aud": "api", "exp": now.Add(time.Minute).Unix(),
	})
	expired := token(t, []byte(secret), jwt.SigningMethodHS256, jwt.MapClaims{"sub": "u-1", "iss": "x", "aud": "api", "exp": now.Add(-time.Minute).Unix()})
	noExp := token(t, []byte(secret), jwt.SigningMethodHS256, jwt.MapClaims{"sub": "u-1", "iss": "x", "aud": "api"})
	wrongIss := token(t, []byte(secret), jwt.SigningMethodHS256, jwt.MapClaims{"sub": "u-1", "iss": "y", "aud": "api", "exp": now.Add(time.Minute).Unix()})
	wrongKey := token(t, []byte("another-secret-another-secret-00"), jwt.SigningMethodHS256, jwt.MapClaims{"sub": "u-1", "iss": "x", "aud": "api", "exp": now.Add(time.Minute).Unix()})
	noSub := token(t, []byte(secret), jwt.SigningMethodHS256, jwt.MapClaims{"iss": "x", "aud": "api", "exp": now.Add(time.Minute).Unix()})
	cfg := func(mode string) config.Auth {
		return config.Auth{Mode: mode, APIKeys: keys, JWT: config.JWT{HS256Secret: secret, Issuer: "x", Audience: "api"}, Public: []string{"/a.v1.S/Public"}}
	}

	tests := []struct {
		name     string
		mode     string
		proc     string
		headers  map[string]string
		wantSub  string
		wantCode string // x-error-code, "" = success
	}{
		{"required, nothing", config.AuthRequired, "/a.v1.S/Get", nil, "", server.UnauthenticatedCode},
		{"optional, nothing", config.AuthOptional, "/a.v1.S/Get", nil, "", ""},
		{"required, public procedure", config.AuthRequired, "/a.v1.S/Public", nil, "", ""},
		{"api key", config.AuthRequired, "/a.v1.S/Get", map[string]string{server.APIKeyHeader: "console-key-0123456789"}, "console", ""},
		{"api key unknown", config.AuthRequired, "/a.v1.S/Get", map[string]string{server.APIKeyHeader: "nope-nope-nope-nope"}, "", server.InvalidCredentialCode},
		{"api key unknown, optional is still an error", config.AuthOptional, "/a.v1.S/Get", map[string]string{server.APIKeyHeader: "nope-nope-nope-nope"}, "", server.InvalidCredentialCode},
		{"jwt", config.AuthRequired, "/a.v1.S/Get", map[string]string{server.AuthorizationHeader: "Bearer " + valid}, "u-1", ""},
		{"jwt lowercase scheme", config.AuthRequired, "/a.v1.S/Get", map[string]string{server.AuthorizationHeader: "bearer " + valid}, "u-1", ""},
		{"jwt expired", config.AuthRequired, "/a.v1.S/Get", map[string]string{server.AuthorizationHeader: "Bearer " + expired}, "", server.InvalidCredentialCode},
		{"jwt without exp", config.AuthRequired, "/a.v1.S/Get", map[string]string{server.AuthorizationHeader: "Bearer " + noExp}, "", server.InvalidCredentialCode},
		{"jwt wrong issuer", config.AuthRequired, "/a.v1.S/Get", map[string]string{server.AuthorizationHeader: "Bearer " + wrongIss}, "", server.InvalidCredentialCode},
		{"jwt wrong key", config.AuthRequired, "/a.v1.S/Get", map[string]string{server.AuthorizationHeader: "Bearer " + wrongKey}, "", server.InvalidCredentialCode},
		{"jwt without sub", config.AuthRequired, "/a.v1.S/Get", map[string]string{server.AuthorizationHeader: "Bearer " + noSub}, "", server.InvalidCredentialCode},
		{"not a bearer", config.AuthRequired, "/a.v1.S/Get", map[string]string{server.AuthorizationHeader: "Basic abc"}, "", server.InvalidCredentialCode},
		{"garbage token", config.AuthOptional, "/a.v1.S/Get", map[string]string{server.AuthorizationHeader: "Bearer not.a.jwt"}, "", server.InvalidCredentialCode},
		{"api key wins over bearer", config.AuthRequired, "/a.v1.S/Get", map[string]string{server.APIKeyHeader: "ci-key-0123456789abcdef", server.AuthorizationHeader: "Bearer " + expired}, "ci", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := call(t, cfg(tt.mode), tt.proc, tt.headers)
			if tt.wantCode != "" {
				code, ec := errCode(err)
				if code != connect.CodeUnauthenticated || ec != tt.wantCode {
					t.Fatalf("err = %v (code %s, x-error-code %q), want unauthenticated %s", err, code, ec, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if p.Subject != tt.wantSub {
				t.Fatalf("principal = %+v, want sub %q", p, tt.wantSub)
			}
			if tt.wantSub == "u-1" && (p.Name != "Ann" || !p.HasRole("admin") || p.Method != server.MethodJWT) {
				t.Fatalf("jwt principal = %+v", p)
			}
			if tt.wantSub == "console" && (p.Method != server.MethodAPIKey || len(p.Roles) != 0) {
				t.Fatalf("api key principal = %+v", p)
			}
			if tt.wantSub == "ci" && (!p.HasRole("admin") || !p.HasRole("ops") || p.HasRole("root")) {
				t.Fatalf("api key roles = %+v", p)
			}
		})
	}
}

// The algorithm family is pinned to the key: with an HS256 secret only HS256
// verifies; with a public key, a token HMAC-signed with the public key's PEM
// (the classic alg-confusion attack) is rejected.
func TestAuthInterceptor_AlgorithmPinned(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(pub)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	exp := time.Now().Add(time.Minute).Unix()
	cfg := config.Auth{Mode: config.AuthRequired, JWT: config.JWT{PublicKey: pemKey}}

	good := token(t, priv, jwt.SigningMethodEdDSA, jwt.MapClaims{"sub": "u-2", "exp": exp})
	if p, err := call(t, cfg, "/a.v1.S/Get", map[string]string{server.AuthorizationHeader: "Bearer " + good}); err != nil || p.Subject != "u-2" {
		t.Fatalf("EdDSA token: %+v, %v", p, err)
	}
	confused := token(t, []byte(pemKey), jwt.SigningMethodHS256, jwt.MapClaims{"sub": "u-2", "exp": exp})
	if _, err := call(t, cfg, "/a.v1.S/Get", map[string]string{server.AuthorizationHeader: "Bearer " + confused}); err == nil {
		t.Fatal("HS256 token signed with the public key must be rejected")
	}
	if _, err := server.NewAuthInterceptor(config.Auth{Mode: config.AuthRequired, JWT: config.JWT{PublicKey: "not pem"}}, noop.NewMeterProvider(), nil); err == nil {
		t.Fatal("bad pem must fail")
	}
	// Only api keys configured: a bearer token is a credential that cannot verify.
	_, err = call(t, config.Auth{Mode: config.AuthOptional, APIKeys: "ci=ci-key-0123456789abcdef"}, "/a.v1.S/Get", map[string]string{server.AuthorizationHeader: "Bearer " + good})
	if code, ec := errCode(err); code != connect.CodeUnauthenticated || ec != server.InvalidCredentialCode {
		t.Fatalf("bearer with only api keys = %v", err)
	}
}

func TestRequireRole(t *testing.T) {
	c := context.Background()
	admin := contextx.WithPrincipal(c, contextx.Principal{Subject: "a", Roles: []string{"admin"}})
	user := contextx.WithPrincipal(c, contextx.Principal{Subject: "u"})
	off := config.Auth{Mode: config.AuthOff}
	on := config.Auth{Mode: config.AuthRequired}

	if err := server.RequireRole(c, off, "admin"); err != nil {
		t.Fatalf("auth off must allow: %v", err)
	}
	if err := server.RequireRole(admin, on, "admin"); err != nil {
		t.Fatalf("admin must pass: %v", err)
	}
	if code, ec := errCode(server.RequireRole(user, on, "admin")); code != connect.CodePermissionDenied || ec != server.PermissionDeniedCode {
		t.Fatalf("user = %s %s", code, ec)
	}
	if code, ec := errCode(server.RequireRole(c, on, "admin")); code != connect.CodeUnauthenticated || ec != server.UnauthenticatedCode {
		t.Fatalf("anonymous = %s %s", code, ec)
	}
}

// resolver is a CredentialResolver with a fixed answer table and a switchable failure.
type resolver struct {
	known map[string]contextx.Principal
	fail  bool
	calls int
}

func (r *resolver) ResolveAPIKey(_ context.Context, key string) (contextx.Principal, bool, error) {
	r.calls++
	if r.fail {
		return contextx.Principal{}, false, errors.New("db down")
	}
	p, ok := r.known[key]

	return p, ok, nil
}

// Keys the config does not know are asked of the resolver; the config keys
// win first (break glass) and never hit it; a resolver failure rejects the
// request as invalid instead of letting it through; and a resolver alone is a
// valid auth configuration.
func TestAuthInterceptor_Resolver(t *testing.T) {
	r := &resolver{known: map[string]contextx.Principal{
		"xk_dbkey-0123456789abcdef0123456789": {Subject: "acc-1", Name: "ops", Roles: []string{"admin"}, CredentialID: "key-1"},
	}}
	cfg := config.Auth{Mode: config.AuthRequired, APIKeys: "ci=ci-key-0123456789abcdef:viewer"}
	callWith := func(key string) (contextx.Principal, error) {
		in, err := server.NewAuthInterceptor(cfg, noop.NewMeterProvider(), r)
		if err != nil {
			t.Fatal(err)
		}
		var seen contextx.Principal
		next := in(func(c context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
			seen = contextx.PrincipalFrom(c)
			return connect.NewResponse(&struct{}{}), nil
		})
		req := connect.NewRequest(&struct{}{})
		req.Header().Set(server.APIKeyHeader, key)
		_, err = next(context.Background(), request{AnyRequest: req, procedure: "/x.v1.X/Do"})

		return seen, err
	}

	p, err := callWith("ci-key-0123456789abcdef")
	if err != nil || p.Subject != "ci" || r.calls != 0 {
		t.Fatalf("config key: %+v, %v, resolver calls %d", p, err, r.calls)
	}
	p, err = callWith("xk_dbkey-0123456789abcdef0123456789")
	if err != nil || p.Subject != "acc-1" || p.Method != server.MethodAPIKey || p.CredentialID != "key-1" || !p.HasRole("admin") || r.calls != 1 {
		t.Fatalf("resolved key: %+v, %v, calls %d", p, err, r.calls)
	}
	if _, err := callWith("xk_unknown-0123456789abcdef01234567"); !isInvalidCredential(err) {
		t.Fatalf("unknown key: %v", err)
	}
	r.fail = true
	_, err = callWith("xk_dbkey-0123456789abcdef0123456789")
	if !isInvalidCredential(err) {
		t.Fatalf("resolver failure: %v", err)
	}
	if strings.Contains(err.Error(), "db down") {
		t.Fatalf("resolver failure must not leak to the client: %v", err)
	}
	r.fail = false

	// Resolver only: no static key, no jwt, still a valid configuration.
	if _, err := server.NewAuthInterceptor(config.Auth{Mode: config.AuthRequired}, noop.NewMeterProvider(), r); err != nil {
		t.Fatalf("resolver-only config: %v", err)
	}
}

func isInvalidCredential(err error) bool {
	var ce *connect.Error
	return errors.As(err, &ce) && ce.Code() == connect.CodeUnauthenticated && ce.Meta().Get("x-error-code") == server.InvalidCredentialCode
}
