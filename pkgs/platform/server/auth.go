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
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
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

// CredentialResolver is the platform's door to credentials that live
// somewhere other than the config: the identity bounded context implements it
// (its adapters/auth) and the composition root wires it in, so the platform
// never imports a BC. ResolveAPIKey answers for a presented X-API-Key the
// static auth.api_keys did not match: found=false means no such key (the
// request is rejected as invalid), err means the lookup itself failed (the
// request is rejected too, and the error is logged — a database outage must
// not log anyone in). The resolver owns its cache and its hashing.
type CredentialResolver interface {
	ResolveAPIKey(c context.Context, key string) (p contextx.Principal, found bool, err error)
}

// authenticator verifies one kind of credential already extracted from the
// request. Verifying is constant-time where the credential is a secret.
type authenticator struct {
	keys            map[[32]byte]apiKey // sha256(key) → who (config auth.api_keys)
	resolver        CredentialResolver  // keys managed elsewhere, asked after keys
	resolverTimeout time.Duration       // bound on one resolver call (auth.resolver_timeout; 0 = none)
	jwt             *jwtVerifier
}

type apiKey struct {
	name  string
	roles []string
}

// Results of one pass through the auth interceptor, the `result` attribute of
// auth.decisions (Prometheus: auth_decisions_total{result, method}). The
// rejected ones are what the dashboard's Auth row and the "credential
// rejections" alert watch: `invalid` climbing is someone guessing keys or a
// client still sending a rotated key, `missing` climbing after a deploy is a
// client that lost its header. otelconnect's rpc_server_call_duration_seconds
// only says `unauthenticated` for both, hence this counter. `method` is which
// credential header was sent (api_key / jwt / none) — never who.
const (
	ResultOK        = "ok"        // credential verified, principal in the context
	ResultPublic    = "public"    // procedure in auth.public, nothing checked
	ResultAnonymous = "anonymous" // no credential, optional mode: runs anonymous
	ResultMissing   = "missing"   // no credential, required mode: Unauthenticated
	ResultInvalid   = "invalid"   // credential sent and rejected (any mode)

	// MethodNone is the `method` attribute when no credential header was sent.
	MethodNone = "none"
)

// NewAuthInterceptor builds the interceptor for cfg. With auth.mode off it
// returns nil (nothing to install). Otherwise every RPC is resolved to a
// principal: X-API-Key against auth.api_keys and then the resolver (nil when
// no BC manages keys; the static keys stay the break-glass path that needs no
// database), else Authorization: Bearer against auth.jwt; a credential that is present but does not verify is
// always an error (even in optional mode — a bad token is never "anonymous"),
// a missing one is anonymous in optional mode and Unauthenticated in required
// mode. auth.public procedures skip all of it. The principal goes into the
// context, into the request logger as `sub` (use-case lines), onto the rpc
// access line (`auth`, `sub`, see NewLoggingInterceptor) and onto the span
// (enduser.id, enduser.roles, auth.method) — the audit trail across Loki and
// Tempo. Names and credentials are never recorded. Every decision is counted
// on mp's auth.decisions{result, method} (see the Result* constants).
func NewAuthInterceptor(cfg config.Auth, mp metric.MeterProvider, resolver CredentialResolver, oidcAuth OIDCAuth) (connect.UnaryInterceptorFunc, error) {
	switch cfg.Mode {
	case config.AuthOff, "":
		return nil, nil
	case config.AuthOptional, config.AuthRequired:
	default:
		return nil, fmt.Errorf("auth: unknown mode %q", cfg.Mode)
	}
	decisions, err := mp.Meter("github.com/blackhorseya/x-monorepo/pkgs/platform/server").Int64Counter(
		"auth.decisions",
		metric.WithDescription("Auth interceptor outcomes: result ok / public / anonymous / missing / invalid, by credential method api_key / jwt / none."),
	)
	if err != nil {
		return nil, fmt.Errorf("auth: decisions counter: %w", err)
	}
	a := &authenticator{resolver: resolver, resolverTimeout: cfg.ResolverTimeout}
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
	if a.keys == nil && a.jwt == nil && a.resolver == nil {
		return nil, errors.New("auth: mode is on but neither api_keys nor jwt is configured")
	}
	public := make(map[string]bool, len(cfg.Public))
	for _, p := range cfg.Public {
		public[p] = true
	}
	required := cfg.Mode == config.AuthRequired

	count := func(c context.Context, result, method string) {
		decisions.Add(c, 1, metric.WithAttributes(attribute.String("result", result), attribute.String("method", method)))
	}

	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(c context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if public[req.Spec().Procedure] {
				count(c, ResultPublic, MethodNone)
				recordAuth(c, AuthPublic, "")
				trace.SpanFromContext(c).SetAttributes(authMethodKey.String(AuthPublic))

				return next(c, req)
			}
			apiKey, authorization := req.Header().Get(APIKeyHeader), req.Header().Get(AuthorizationHeader)
			p, found, err := a.authenticate(c, apiKey, authorization)
			switch {
			case errors.Is(err, errResolver):
				// The lookup failed, not the credential: say so in the log (the
				// client sees the same rejection as a bad key), count it as invalid.
				count(c, ResultInvalid, credentialMethod(apiKey, authorization))
				contextx.FromContext(c).Error("auth: credential resolver failed", "error", err)

				return nil, authError(connect.CodeUnauthenticated, InvalidCredentialCode, errors.New("credential could not be verified"))
			case err != nil:
				count(c, ResultInvalid, credentialMethod(apiKey, authorization))

				return nil, authError(connect.CodeUnauthenticated, InvalidCredentialCode, err)
			case !found && required:
				count(c, ResultMissing, MethodNone)

				return nil, authError(connect.CodeUnauthenticated, UnauthenticatedCode, errors.New("credentials required"))
			case !found:
				count(c, ResultAnonymous, MethodNone)
				trace.SpanFromContext(c).SetAttributes(authMethodKey.String(AuthAnonymous))

				return next(c, req)
			}
			count(c, ResultOK, p.Method)
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

// credentialMethod names the credential a request carried, with the same
// precedence as authenticate (an api key wins over a bearer token).
func credentialMethod(apiKey, authorization string) string {
	switch {
	case apiKey != "":
		return MethodAPIKey
	case authorization != "":
		return MethodJWT
	default:
		return MethodNone
	}
}

// errResolver marks a CredentialResolver failure (infrastructure, not the credential).
var errResolver = errors.New("credential resolver")

// authenticate resolves the headers to a principal. found is false when no
// credential was sent; err is set when one was sent and is invalid (or, wrapped
// in errResolver, when the resolver could not answer).
func (a *authenticator) authenticate(c context.Context, apiKey, authorization string) (contextx.Principal, bool, error) {
	if apiKey != "" {
		if a.keys == nil && a.resolver == nil {
			return contextx.Principal{}, true, errors.New("api keys are not accepted")
		}
		if k, ok := a.keys[sha256.Sum256([]byte(apiKey))]; ok {
			return contextx.Principal{Subject: k.name, Roles: k.roles, Method: MethodAPIKey}, true, nil
		}
		if a.resolver != nil {
			// Auth sits outside the timeout interceptor (the deadline is for
			// validation + use case), so the resolver's database read gets
			// its own bound: a stuck database rejects fast instead of holding
			// every request and the shutdown drain.
			rc, cancel := c, func() {}
			if a.resolverTimeout > 0 {
				rc, cancel = context.WithTimeout(c, a.resolverTimeout)
			}
			p, ok, err := a.resolver.ResolveAPIKey(rc, apiKey)
			deadlineErr := rc.Err() // read BEFORE cancel(): cancel() itself makes rc.Err() non-nil
			cancel()
			switch {
			case err != nil:
				return contextx.Principal{}, true, fmt.Errorf("%w: %w", errResolver, err)
			case deadlineErr != nil:
				// The deadline passed while the resolver was answering: a late
				// success is not a verified credential (the resolver may have
				// ignored the context, or won a race with it). The bound is only
				// a bound for resolvers that honour their context; this keeps
				// the decision honest either way.
				return contextx.Principal{}, true, fmt.Errorf("%w: %w", errResolver, deadlineErr)
			case ok:
				p.Method = MethodAPIKey

				return p, true, nil
			}
		}

		return contextx.Principal{}, true, errors.New("unknown api key")
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
