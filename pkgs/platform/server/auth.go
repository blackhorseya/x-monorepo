package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// Authentication is transport-level, like validation: the interceptor
// establishes who the call runs as (contextx.Principal) and nothing more.
// What a principal may do is decided by whoever reads it. The probes,
// /api/metrics and /api/docs are gin routes outside the Connect chain and
// stay open whatever the mode.
const (
	// APIKeyHeader carries a static key (config auth.api_keys).
	APIKeyHeader = "X-API-Key"
	// AuthorizationHeader carries "Bearer <jwt>".
	AuthorizationHeader = "Authorization"

	// UnauthenticatedCode is the x-error-code when a credential is required
	// and none was sent.
	UnauthenticatedCode = "auth.unauthenticated"
	// InvalidCredentialCode is the x-error-code when a credential was sent
	// and does not verify (bad key, bad signature, expired, wrong iss/aud).
	InvalidCredentialCode = "auth.invalid_credential"

	// Principal methods.
	MethodJWT    = "jwt"
	MethodAPIKey = "api_key"
)

// authenticator verifies one kind of credential already extracted from the
// request. Verifying is constant-time where the credential is a secret.
type authenticator struct {
	keys map[[32]byte]apiKey // sha256(key) → who
	jwt  *jwtVerifier
}

type apiKey struct {
	name  string
	roles []string
}

// NewAuthInterceptor builds the interceptor for cfg. With auth.mode off it
// returns nil (nothing to install). Otherwise every RPC is resolved to a
// principal: X-API-Key against auth.api_keys, else Authorization: Bearer
// against auth.jwt; a credential that is present but does not verify is
// always an error (even in optional mode — a bad token is never "anonymous"),
// a missing one is anonymous in optional mode and Unauthenticated in required
// mode. auth.public procedures skip all of it. The principal goes into the
// context, into the request logger as `sub` (use-case lines), onto the rpc
// access line (`auth`, `sub`, see NewLoggingInterceptor) and onto the span
// (enduser.id, enduser.roles, auth.method) — the audit trail across Loki and
// Tempo. Names and credentials are never recorded.
func NewAuthInterceptor(cfg config.Auth) (connect.UnaryInterceptorFunc, error) {
	switch cfg.Mode {
	case config.AuthOff, "":
		return nil, nil
	case config.AuthOptional, config.AuthRequired:
	default:
		return nil, fmt.Errorf("auth: unknown mode %q", cfg.Mode)
	}
	a := &authenticator{}
	if cfg.APIKeys != "" {
		keys, err := parseAPIKeys(cfg.APIKeys)
		if err != nil {
			return nil, err
		}
		a.keys = keys
	}
	if cfg.JWT.HS256Secret != "" || cfg.JWT.PublicKey != "" {
		v, err := newJWTVerifier(cfg.JWT)
		if err != nil {
			return nil, err
		}
		a.jwt = v
	}
	if a.keys == nil && a.jwt == nil {
		return nil, errors.New("auth: mode is on but neither api_keys nor jwt is configured")
	}
	public := make(map[string]bool, len(cfg.Public))
	for _, p := range cfg.Public {
		public[p] = true
	}
	required := cfg.Mode == config.AuthRequired

	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(c context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if public[req.Spec().Procedure] {
				recordAuth(c, AuthPublic, "")
				trace.SpanFromContext(c).SetAttributes(authMethodKey.String(AuthPublic))

				return next(c, req)
			}
			p, found, err := a.authenticate(req.Header().Get(APIKeyHeader), req.Header().Get(AuthorizationHeader))
			switch {
			case err != nil:
				return nil, authError(connect.CodeUnauthenticated, InvalidCredentialCode, err)
			case !found && required:
				return nil, authError(connect.CodeUnauthenticated, UnauthenticatedCode, errors.New("credentials required"))
			case !found:
				trace.SpanFromContext(c).SetAttributes(authMethodKey.String(AuthAnonymous))

				return next(c, req)
			}
			recordAuth(c, p.Method, p.Subject)
			trace.SpanFromContext(c).SetAttributes(
				semconv.EnduserID(p.Subject),
				enduserRolesKey.StringSlice(p.Roles),
				authMethodKey.String(p.Method),
			)
			ctx := contextx.FromContext(c)
			ctx = contextx.FromContext(contextx.WithLogger(ctx, ctx.With("sub", p.Subject)))

			return next(contextx.WithPrincipal(ctx, p), req)
		}
	}, nil
}

// Span attributes set by the auth interceptor. enduser.id is the semantic
// convention; the other two are ours (enduser.role was dropped from semconv).
const (
	enduserRolesKey = attribute.Key("enduser.roles")
	authMethodKey   = attribute.Key("auth.method")
)

// authenticate resolves the headers to a principal. found is false when no
// credential was sent; err is set when one was sent and is invalid.
func (a *authenticator) authenticate(apiKey, authorization string) (contextx.Principal, bool, error) {
	if apiKey != "" {
		if a.keys == nil {
			return contextx.Principal{}, true, errors.New("api keys are not accepted")
		}
		k, ok := a.keys[sha256.Sum256([]byte(apiKey))]
		if !ok {
			return contextx.Principal{}, true, errors.New("unknown api key")
		}

		return contextx.Principal{Subject: k.name, Roles: k.roles, Method: MethodAPIKey}, true, nil
	}
	if authorization != "" {
		scheme, token, _ := strings.Cut(authorization, " ")
		if !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
			return contextx.Principal{}, true, errors.New("authorization header is not a bearer token")
		}
		if a.jwt == nil {
			return contextx.Principal{}, true, errors.New("bearer tokens are not accepted")
		}
		p, err := a.jwt.verify(strings.TrimSpace(token))

		return p, true, err
	}

	return contextx.Principal{}, false, nil
}

// parseAPIKeys reads "name=key[:role|role…][,name=key…]". Keys are kept and
// looked up as SHA-256 digests, so the comparison does not leak the key by
// timing and a config dump does not show it.
func parseAPIKeys(s string) (map[[32]byte]apiKey, error) {
	keys := map[[32]byte]apiKey{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, rest, ok := strings.Cut(pair, "=")
		key, roleList, _ := strings.Cut(rest, ":")
		name, key = strings.TrimSpace(name), strings.TrimSpace(key)
		if !ok || name == "" || len(key) < 16 {
			return nil, fmt.Errorf("auth: api_keys entry %q: want name=key[:role|role] with a key of at least 16 characters", name)
		}
		var roles []string
		for _, r := range strings.Split(roleList, "|") {
			if r = strings.TrimSpace(r); r != "" {
				roles = append(roles, r)
			}
		}
		keys[sha256.Sum256([]byte(key))] = apiKey{name: name, roles: roles}
	}
	if len(keys) == 0 {
		return nil, errors.New("auth: api_keys is set but holds no name=key entry")
	}

	return keys, nil
}

// jwtVerifier pins the algorithm family to the configured key so an HS256
// token cannot be verified against a public key used as the HMAC secret.
type jwtVerifier struct {
	key     any
	methods []string
	opts    []jwt.ParserOption
}

type claims struct {
	jwt.RegisteredClaims
	Name  string   `json:"name,omitempty"`
	Roles []string `json:"roles,omitempty"`
}

func newJWTVerifier(cfg config.JWT) (*jwtVerifier, error) {
	v := &jwtVerifier{opts: []jwt.ParserOption{jwt.WithExpirationRequired(), jwt.WithLeeway(0)}}
	switch {
	case cfg.HS256Secret != "" && cfg.PublicKey != "":
		return nil, errors.New("auth: jwt.hs256_secret and jwt.public_key are exclusive")
	case cfg.HS256Secret != "":
		if len(cfg.HS256Secret) < 32 {
			return nil, errors.New("auth: jwt.hs256_secret must be at least 32 bytes")
		}
		v.key, v.methods = []byte(cfg.HS256Secret), []string{"HS256"}
	default:
		block, _ := pem.Decode([]byte(cfg.PublicKey))
		if block == nil {
			return nil, errors.New("auth: jwt.public_key is not PEM")
		}
		key, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("auth: jwt.public_key: %w", err)
		}
		v.key = key
		switch key.(type) {
		case *rsa.PublicKey:
			v.methods = []string{"RS256", "RS384", "RS512"}
		case *ecdsa.PublicKey:
			v.methods = []string{"ES256", "ES384", "ES512"}
		case ed25519.PublicKey:
			v.methods = []string{"EdDSA"}
		default:
			return nil, fmt.Errorf("auth: jwt.public_key: unsupported key type %T", key)
		}
	}
	v.opts = append(v.opts, jwt.WithValidMethods(v.methods))
	if cfg.Issuer != "" {
		v.opts = append(v.opts, jwt.WithIssuer(cfg.Issuer))
	}
	if cfg.Audience != "" {
		v.opts = append(v.opts, jwt.WithAudience(cfg.Audience))
	}

	return v, nil
}

func (v *jwtVerifier) verify(token string) (contextx.Principal, error) {
	var cl claims
	_, err := jwt.ParseWithClaims(token, &cl, func(*jwt.Token) (any, error) { return v.key, nil }, v.opts...)
	if err != nil {
		return contextx.Principal{}, fmt.Errorf("invalid token: %w", err)
	}
	if cl.Subject == "" {
		return contextx.Principal{}, errors.New("invalid token: no sub")
	}

	return contextx.Principal{Subject: cl.Subject, Name: cl.Name, Roles: cl.Roles, Method: MethodJWT}, nil
}

// PermissionDeniedCode is the x-error-code when the principal is known but
// lacks what the procedure requires (see RequireRole).
const PermissionDeniedCode = "auth.permission_denied"

// RequireRole is the one authorization helper the platform offers, for its
// own admin surface: with auth off it allows everything (the deployment chose
// no auth); otherwise the principal must exist and hold role. Returns a
// connect error ready to be returned from a handler.
func RequireRole(c context.Context, cfg config.Auth, role string) error {
	if cfg.Mode == config.AuthOff || cfg.Mode == "" {
		return nil
	}
	p := contextx.PrincipalFrom(c)
	if p.IsAnonymous() {
		return authError(connect.CodeUnauthenticated, UnauthenticatedCode, errors.New("credentials required"))
	}
	if !p.HasRole(role) {
		return authError(connect.CodePermissionDenied, PermissionDeniedCode, fmt.Errorf("role %q required", role))
	}

	return nil
}

func authError(code connect.Code, errorCode string, err error) error {
	ce := connect.NewError(code, err)
	ce.Meta().Set("x-error-code", errorCode)

	return ce
}
