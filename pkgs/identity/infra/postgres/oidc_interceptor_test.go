package postgres_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/adapters/auth"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/adapters/rpc"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	identityv1 "github.com/blackhorseya/x-monorepo/pkgs/identity/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/contract/v1/identityv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/oidc"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// The whole path of #272 over a real Postgres, a real OIDC verifier and real
// signatures: token → verifier → resolver → principal. Only the IdP is a test
// server (discovery + JWKS on a loopback port); everything on our side is the
// production code.

const oidcClient = "x-console"

// idp is a test OpenID provider with one Ed25519 key.
type idp struct {
	srv  *httptest.Server
	priv ed25519.PrivateKey
}

func newIdP(t *testing.T) *idp {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p := &idp{priv: priv}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": p.srv.URL, "jwks_uri": p.srv.URL + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "OKP", "kid": "k1", "use": "sig", "alg": "EdDSA", "crv": "Ed25519",
			"x": base64.RawURLEncoding.EncodeToString(pub),
		}}})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)

	return p
}

func (p *idp) token(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(p.priv)
	if err != nil {
		t.Fatal(err)
	}

	return s
}

// flakyResolve is the resolve use case the database can be taken away from.
type flakyResolve struct {
	inner auth.ResolveExternalUC
	down  atomic.Bool
	calls atomic.Int64
	late  atomic.Int64 // nanoseconds the answer is held back, ignoring the context
}

func (f *flakyResolve) Execute(c context.Context, in dto.ResolveExternalInput) (*dto.ResolveExternalOutput, error) {
	f.calls.Add(1)
	if f.down.Load() {
		return nil, errors.New("connection refused")
	}
	out, err := f.inner.Execute(c, in)
	time.Sleep(time.Duration(f.late.Load()))

	return out, err
}

type oidcRequest struct {
	connect.AnyRequest
	procedure string
}

func (r oidcRequest) Spec() connect.Spec { return connect.Spec{Procedure: r.procedure} }

type oidcWorld struct {
	*extWorld
	idp   *idp
	flaky *flakyResolve
	call  func(procedure, token string) (contextx.Principal, error)
	iss   string
}

// seen is what the handler behind the interceptor saw.
type seen struct {
	principal contextx.Principal
	claimed   bool // ClaimInvite ran and bound the identity
}

func newOIDCWorld(t *testing.T, resolverTimeout time.Duration) *oidcWorld {
	t.Helper()
	w := &oidcWorld{extWorld: newExtWorld(t), idp: newIdP(t)}
	w.iss = w.idp.srv.URL
	w.flaky = &flakyResolve{inner: w.resolve}

	v, err := oidc.New(config.OIDC{Enabled: true, Issuers: []config.OIDCIssuer{{Issuer: w.iss, Audience: oidcClient, AllowInsecureIssuer: true}}},
		oidc.WithClock(w.clock.Now), oidc.WithMeterProvider(noop.NewMeterProvider()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.Close)
	ext, err := auth.NewExternalResolver(w.flaky, w.provision, auth.StaticMapping{}, w.clock, noop.NewMeterProvider())
	if err != nil {
		t.Fatal(err)
	}
	in, err := server.NewAuthInterceptor(config.Auth{Mode: config.AuthRequired, AdminRole: "admin", APIKeys: "ops=ops-key-0123456789abc:admin", ResolverTimeout: resolverTimeout}, noop.NewMeterProvider(), nil,
		server.OIDCAuth{Verifier: v, Resolver: auth.NewExternalCredentials(ext)})
	if err != nil {
		t.Fatal(err)
	}
	svc := rpc.NewIdentityService(rpc.UseCases{ClaimInvite: w.claim, ExternalIdentity: rpc.ContextExternalIdentity{}}, config.Auth{AdminRole: "admin"})

	w.call = func(procedure, token string) (contextx.Principal, error) {
		var got seen
		next := in(func(c context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
			got.principal = contextx.PrincipalFrom(c)
			if procedure == identityv1connect.IdentityServiceClaimInviteProcedure {
				if _, err := svc.ClaimInvite(c, connect.NewRequest(&identityv1.ClaimInviteRequest{})); err != nil {
					return nil, err
				}
				got.claimed = true
			}

			return connect.NewResponse(&struct{}{}), nil
		})
		req := connect.NewRequest(&struct{}{})
		req.Header().Set(server.AuthorizationHeader, "Bearer "+token)
		_, err := next(w.ctx, oidcRequest{AnyRequest: req, procedure: procedure})
		if err == nil && procedure == identityv1connect.IdentityServiceClaimInviteProcedure && !got.claimed {
			err = errors.New("ClaimInvite did not run")
		}

		return got.principal, err
	}

	return w
}

// jwt signs claims for sub as of the world's clock.
func (w *oidcWorld) jwt(t *testing.T, sub string, extra jwt.MapClaims) string {
	t.Helper()
	now := w.clock.Now()
	cl := jwt.MapClaims{
		"iss": w.iss, "sub": sub, "aud": oidcClient, "email": sub + "@example.com", "email_verified": true,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	for k, v := range extra {
		cl[k] = v
	}

	return w.idp.token(t, cl)
}

func codeOf(err error) (connect.Code, string) {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return 0, ""
	}

	return ce.Code(), ce.Meta().Get("x-error-code")
}

func (w *oidcWorld) wantRejected(t *testing.T, what, procedure, token string, code connect.Code, errCode string) {
	t.Helper()
	p, err := w.call(procedure, token)
	if gotCode, gotErr := codeOf(err); gotCode != code || gotErr != errCode {
		t.Errorf("%s: %v / %q (%v), want %v / %q", what, gotCode, gotErr, err, code, errCode)
	}
	if !p.IsAnonymous() {
		t.Errorf("%s: a rejected call had principal %+v", what, p)
	}
}

const orderProc = "/ordering.v1.OrderService/CreateOrder"

// An unbound identity may claim its invite and nothing else; once claimed, the
// same token is the account's principal, with the account's roles and not the
// token's.
func TestOIDCInterceptor_UnboundThenClaimedOnPostgres(t *testing.T) {
	w := newOIDCWorld(t, 0)
	w.newAccount(t, "acc-1", "viewer")
	w.invite(t, "acc-1", "sub-1@example.com")
	// The token claims admin; the account does not have it.
	tok := w.jwt(t, "sub-1", jwt.MapClaims{"roles": []string{"admin"}, "role": "admin"})

	w.wantRejected(t, "unbound, other procedure", orderProc, tok, connect.CodePermissionDenied, server.UnboundIdentityCode)
	w.wantRejected(t, "unbound, WhoAmI", identityv1connect.IdentityServiceWhoAmIProcedure, tok, connect.CodePermissionDenied, server.UnboundIdentityCode)
	if n := w.count(t, "SELECT count(*) FROM identity.external_identities"); n != 0 {
		t.Fatalf("a refused call bound %d identities", n)
	}

	p, err := w.call(identityv1connect.IdentityServiceClaimInviteProcedure, tok)
	if err != nil {
		t.Fatalf("ClaimInvite as unbound: %v", err)
	}
	if !p.IsAnonymous() {
		t.Errorf("an unbound identity has no principal, got %+v", p)
	}
	if n := w.count(t, "SELECT count(*) FROM identity.external_identities WHERE account_id = 'acc-1'"); n != 1 {
		t.Fatalf("bindings after ClaimInvite = %d, want 1", n)
	}

	p, err = w.call(orderProc, tok)
	if err != nil {
		t.Fatalf("after claim: %v", err)
	}
	if p.Subject != "acc-1" || p.Method != server.MethodJWT || !p.HasRole("viewer") || p.HasRole("admin") {
		t.Fatalf("principal = %+v, want acc-1 with the account's roles only", p)
	}
}

// A disabled account and a session revoked after the token was issued are
// refused; the database error is refused and not remembered.
func TestOIDCInterceptor_DisabledRevokedAndDBErrorOnPostgres(t *testing.T) {
	w := newOIDCWorld(t, 0)
	w.newAccount(t, "acc-1", "viewer")
	w.invite(t, "acc-1", "sub-1@example.com")
	if _, err := w.claim.Execute(w.ctx, dto.ClaimInviteInput{Issuer: w.iss, Subject: "sub-1", Email: "sub-1@example.com", EmailVerified: true}); err != nil {
		t.Fatal(err)
	}
	old := w.jwt(t, "sub-1", nil)
	if _, err := w.call(orderProc, old); err != nil {
		t.Fatalf("bound: %v", err)
	}

	// The database goes away between cache refreshes: the lookup fails, is
	// refused, and the next request after recovery is served straight away.
	w.clock.advance(auth.DefaultCacheTTL)
	w.flaky.down.Store(true)
	w.wantRejected(t, "db down", orderProc, w.jwt(t, "sub-1", nil), connect.CodeUnauthenticated, server.InvalidCredentialCode)
	w.wantRejected(t, "db still down", orderProc, w.jwt(t, "sub-1", nil), connect.CodeUnauthenticated, server.InvalidCredentialCode)
	w.flaky.down.Store(false)
	if _, err := w.call(orderProc, w.jwt(t, "sub-1", nil)); err != nil {
		t.Fatalf("after recovery: %v", err)
	}
	calls := w.flaky.calls.Load()
	if _, err := w.call(orderProc, w.jwt(t, "sub-1", nil)); err != nil || w.flaky.calls.Load() != calls {
		t.Fatalf("the success is cached (calls %d → %d, err %v)", calls, w.flaky.calls.Load(), err)
	}

	// Revoke all sessions: tokens issued before are refused once the cache turns over; a fresh one is fine.
	w.clock.advance(time.Minute)
	if _, err := w.revoke.Execute(w.ctx, dto.RevokeSessionsInput{AccountID: "acc-1"}); err != nil {
		t.Fatal(err)
	}
	w.clock.advance(auth.DefaultCacheTTL)
	w.wantRejected(t, "iat before sessions_revoked_at", orderProc, old, connect.CodeUnauthenticated, server.InvalidCredentialCode)
	if _, err := w.call(orderProc, w.jwt(t, "sub-1", nil)); err != nil {
		t.Fatalf("token issued after the revocation: %v", err)
	}

	// Disabled: refused within the cache TTL, whatever the token's age.
	if _, err := w.disable.Execute(w.ctx, dto.DisableAccountInput{ID: "acc-1"}); err != nil {
		t.Fatal(err)
	}
	w.clock.advance(auth.DefaultCacheTTL)
	w.wantRejected(t, "disabled", orderProc, w.jwt(t, "sub-1", nil), connect.CodeUnauthenticated, server.InvalidCredentialCode)
}

// An answer that arrives after auth.resolver_timeout is not trusted, even
// "unbound": it must not reach ClaimInvite nor write a binding (reviewer F1 of #290).
func TestOIDCInterceptor_LateUnboundDoesNotClaimOnPostgres(t *testing.T) {
	w := newOIDCWorld(t, 150*time.Millisecond)
	w.newAccount(t, "acc-1", "viewer")
	w.invite(t, "acc-1", "sub-1@example.com")
	w.flaky.late.Store(int64(500 * time.Millisecond))

	w.wantRejected(t, "late unbound, ClaimInvite", identityv1connect.IdentityServiceClaimInviteProcedure, w.jwt(t, "sub-1", nil), connect.CodeUnauthenticated, server.InvalidCredentialCode)
	w.wantRejected(t, "late unbound, other procedure", orderProc, w.jwt(t, "sub-1", nil), connect.CodeUnauthenticated, server.InvalidCredentialCode)
	if n := w.count(t, "SELECT count(*) FROM identity.external_identities"); n != 0 {
		t.Fatalf("a late answer bound %d identities", n)
	}

	// In time, the same call claims the invite.
	w.flaky.late.Store(0)
	if _, err := w.call(identityv1connect.IdentityServiceClaimInviteProcedure, w.jwt(t, "sub-1", nil)); err != nil {
		t.Fatalf("ClaimInvite in time: %v", err)
	}
}
