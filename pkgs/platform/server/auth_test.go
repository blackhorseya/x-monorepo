package server_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

const secret = "0123456789abcdef0123456789abcdef" // 32 bytes

// call runs the interceptor on one fake unary request and returns the
// principal the next function saw, and the error.
func call(t *testing.T, cfg config.Auth, procedure string, headers map[string]string) (contextx.Principal, error) {
	t.Helper()
	in, err := server.NewAuthInterceptor(cfg)
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
		in, err := server.NewAuthInterceptor(config.Auth{Mode: mode})
		if err != nil || in != nil {
			t.Fatalf("mode %q: %v, %v", mode, in, err)
		}
	}
	if _, err := server.NewAuthInterceptor(config.Auth{Mode: config.AuthRequired}); err == nil {
		t.Fatal("required with nothing configured must fail")
	}
	if _, err := server.NewAuthInterceptor(config.Auth{Mode: config.AuthRequired, APIKeys: "ci=short"}); err == nil {
		t.Fatal("short api key must fail")
	}
	if _, err := server.NewAuthInterceptor(config.Auth{Mode: config.AuthRequired, JWT: config.JWT{HS256Secret: "short"}}); err == nil {
		t.Fatal("short hs256 secret must fail")
	}
}

func TestAuthInterceptor_Modes(t *testing.T) {
	keys := "ci=ci-key-0123456789abcdef, console = console-key-0123456789"
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
			if tt.wantSub == "console" && p.Method != server.MethodAPIKey {
				t.Fatalf("api key principal = %+v", p)
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
	if _, err := server.NewAuthInterceptor(config.Auth{Mode: config.AuthRequired, JWT: config.JWT{PublicKey: "not pem"}}); err == nil {
		t.Fatal("bad pem must fail")
	}
	// Only api keys configured: a bearer token is a credential that cannot verify.
	_, err = call(t, config.Auth{Mode: config.AuthOptional, APIKeys: "ci=ci-key-0123456789abcdef"}, "/a.v1.S/Get", map[string]string{server.AuthorizationHeader: "Bearer " + good})
	if code, ec := errCode(err); code != connect.CodeUnauthenticated || ec != server.InvalidCredentialCode {
		t.Fatalf("bearer with only api keys = %v", err)
	}
}
