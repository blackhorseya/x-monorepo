package server_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/oidc"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

const (
	claimInvite = "/identity.v1.IdentityService/ClaimInvite"
	otherProc   = "/ordering.v1.OrderService/CreateOrder"
	oidcToken   = "eyJ.oidc-token-body.sig" // what the fake verifier knows
	apiKeyValue = "ci-key-0123456789abcdef"
)

var extID = oidc.Identity{Issuer: "https://idp.example", Subject: "sub-1", Email: "a@example.com", EmailVerified: true, IssuedAt: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)}

// fakeVerifier knows one token; any other is rejected with err.
type fakeVerifier struct {
	calls int
	err   error // returned for every token when set
}

func (v *fakeVerifier) Verify(_ context.Context, token string) (oidc.Identity, error) {
	v.calls++
	if v.err != nil {
		return oidc.Identity{}, v.err
	}
	if token != oidcToken {
		return oidc.Identity{}, oidc.ErrUnknownIssuer
	}

	return extID, nil
}

// fakeExternal answers ResolveExternal from a canned result.
type fakeExternal struct {
	calls int
	seen  contextx.ExternalIdentity
	p     contextx.Principal
	err   error
	// lateAnswer holds the canned answer back until the context is done, then
	// gives it anyway: a lookup that finished but replied after the deadline (or
	// a driver that ignores its context). Waiting on the context, not on a
	// sleep, makes "the answer is late" true by construction.
	lateAnswer bool
	// honour returns the context's error instead of the canned answer once it is done.
	honour bool
	unbind map[string]bool
}

func (r *fakeExternal) ResolveExternal(c context.Context, id contextx.ExternalIdentity) (contextx.Principal, error) {
	r.calls++
	r.seen = id
	if r.lateAnswer {
		<-c.Done()
		if r.honour {
			return contextx.Principal{}, c.Err()
		}
	}

	return r.p, r.err
}

func (r *fakeExternal) AllowsUnbound(procedure string) bool { return r.unbind[procedure] }

var boundPrincipal = contextx.Principal{Subject: "acc-1", Name: "Ops", Roles: []string{"viewer"}, Method: server.MethodJWT}

// oidcRun is one pass through the interceptor with OIDC wired.
type oidcRun struct {
	principal contextx.Principal
	ext       contextx.ExternalIdentity
	hasExt    bool
	nextCalls int
	err       error
	logs      string
}

func runOIDC(t *testing.T, cfg config.Auth, v server.TokenVerifier, r server.ExternalIdentityResolver, procedure string, headers map[string]string) oidcRun {
	t.Helper()

	return runOIDCCtx(t, context.Background(), cfg, v, r, procedure, headers)
}

// runOIDCCtx is runOIDC with the caller's context (a cancelled one, say).
func runOIDCCtx(t *testing.T, parent context.Context, cfg config.Auth, v server.TokenVerifier, r server.ExternalIdentityResolver, procedure string, headers map[string]string) oidcRun {
	t.Helper()
	in, err := server.NewAuthInterceptor(cfg, noop.NewMeterProvider(), nil, server.OIDCAuth{Verifier: v, Resolver: r})
	if err != nil {
		t.Fatalf("NewAuthInterceptor: %v", err)
	}
	var out oidcRun
	next := in(func(c context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		out.nextCalls++
		out.principal = contextx.PrincipalFrom(c)
		out.ext, out.hasExt = contextx.ExternalIdentityFrom(c)

		return connect.NewResponse(&struct{}{}), nil
	})
	req := connect.NewRequest(&struct{}{})
	for k, val := range headers {
		req.Header().Set(k, val)
	}
	var logs bytes.Buffer
	c := contextx.WithLogger(parent, slog.New(slog.NewJSONHandler(&logs, nil)))
	_, out.err = next(c, request{AnyRequest: req, procedure: procedure})
	out.logs = logs.String()

	return out
}

func bearer(token string) map[string]string {
	return map[string]string{server.AuthorizationHeader: "Bearer " + token}
}

var requiredCfg = config.Auth{Mode: config.AuthRequired, APIKeys: "ci=" + apiKeyValue + ":viewer"}

func wantReject(t *testing.T, out oidcRun, code connect.Code, errCodeWant string) {
	t.Helper()
	if out.nextCalls != 0 {
		t.Errorf("the handler ran for a rejected request (principal %+v)", out.principal)
	}
	if gotCode, gotErr := errCode(out.err); gotCode != code || gotErr != errCodeWant {
		t.Errorf("rejected with %v / %q (%v), want %v / %q", gotCode, gotErr, out.err, code, errCodeWant)
	}
}

// A verified token whose identity is bound becomes the account's principal:
// Subject is the account id, Roles and Name are the account's, Method is jwt.
func TestOIDC_BoundIdentityBecomesAccountPrincipal(t *testing.T) {
	r := &fakeExternal{p: boundPrincipal}
	out := runOIDC(t, requiredCfg, &fakeVerifier{}, r, otherProc, bearer(oidcToken))

	if out.err != nil || out.nextCalls != 1 {
		t.Fatalf("bound identity: err %v, handler ran %d times", out.err, out.nextCalls)
	}
	if out.principal.Subject != "acc-1" || out.principal.Name != "Ops" || out.principal.Method != server.MethodJWT || !out.principal.HasRole("viewer") || out.principal.HasRole("admin") {
		t.Errorf("principal = %+v, want the account's", out.principal)
	}
	if r.seen.Issuer != extID.Issuer || r.seen.Subject != extID.Subject || !r.seen.IssuedAt.Equal(extID.IssuedAt) {
		t.Errorf("resolver was asked about %+v, want the verified identity %+v", r.seen, extID)
	}
}

// OIDC alone is a valid auth configuration (no api_keys, no auth.jwt).
func TestOIDC_AloneIsAValidConfiguration(t *testing.T) {
	in, err := server.NewAuthInterceptor(config.Auth{Mode: config.AuthRequired}, noop.NewMeterProvider(), nil,
		server.OIDCAuth{Verifier: &fakeVerifier{}, Resolver: &fakeExternal{p: boundPrincipal}})
	if err != nil || in == nil {
		t.Fatalf("OIDC alone: %v, %v", in, err)
	}
}

// A token that verifies but is bound to no account may call ClaimInvite and
// nothing else; the handler gets the verified identity from the context, and
// there is no principal.
func TestOIDC_UnboundIdentity(t *testing.T) {
	r := &fakeExternal{err: server.ErrUnboundIdentity, unbind: map[string]bool{claimInvite: true}}

	out := runOIDC(t, requiredCfg, &fakeVerifier{}, r, claimInvite, bearer(oidcToken))
	if out.err != nil || out.nextCalls != 1 {
		t.Fatalf("ClaimInvite: err %v, handler ran %d times", out.err, out.nextCalls)
	}
	if !out.principal.IsAnonymous() {
		t.Errorf("an unbound identity has no principal, got %+v", out.principal)
	}
	if !out.hasExt || out.ext.Issuer != extID.Issuer || out.ext.Subject != extID.Subject || out.ext.Email != extID.Email || !out.ext.EmailVerified {
		t.Errorf("ClaimInvite must see the verified identity in the context, got %+v (present %v)", out.ext, out.hasExt)
	}

	out = runOIDC(t, requiredCfg, &fakeVerifier{}, r, otherProc, bearer(oidcToken))
	wantReject(t, out, connect.CodePermissionDenied, server.UnboundIdentityCode)

	// Optional mode does not turn a verified-but-unbound token into anonymous.
	optional := config.Auth{Mode: config.AuthOptional, APIKeys: requiredCfg.APIKeys}
	out = runOIDC(t, optional, &fakeVerifier{}, r, otherProc, bearer(oidcToken))
	wantReject(t, out, connect.CodePermissionDenied, server.UnboundIdentityCode)
}

// A bound identity calling ClaimInvite is an ordinary principal call: the
// interceptor does not special-case it (the use case refuses a second bind).
func TestOIDC_BoundIdentityCallsClaimInviteAsPrincipal(t *testing.T) {
	r := &fakeExternal{p: boundPrincipal, unbind: map[string]bool{claimInvite: true}}
	out := runOIDC(t, requiredCfg, &fakeVerifier{}, r, claimInvite, bearer(oidcToken))
	if out.err != nil || out.principal.Subject != "acc-1" || !out.hasExt {
		t.Fatalf("bound ClaimInvite: err %v, principal %+v, identity present %v", out.err, out.principal, out.hasExt)
	}
}

// Disabled, revoked or iat-less: the client sees a plain invalid credential,
// never why.
func TestOIDC_RefusedAccountIsInvalidCredential(t *testing.T) {
	r := &fakeExternal{err: fmt.Errorf("%w: account is disabled", server.ErrCredentialRefused)}
	out := runOIDC(t, requiredCfg, &fakeVerifier{}, r, otherProc, bearer(oidcToken))
	wantReject(t, out, connect.CodeUnauthenticated, server.InvalidCredentialCode)
	if out.err != nil && strings.Contains(out.err.Error(), "disabled") {
		t.Errorf("the reason must not reach the client: %v", out.err)
	}
}

// A lookup that could not be made (database down) rejects the request, tells
// the log and not the client, and the next request asks again.
func TestOIDC_ResolverFailureRejects(t *testing.T) {
	r := &fakeExternal{err: errors.New("db down")}
	out := runOIDC(t, requiredCfg, &fakeVerifier{}, r, otherProc, bearer(oidcToken))
	wantReject(t, out, connect.CodeUnauthenticated, server.InvalidCredentialCode)
	if out.err != nil && strings.Contains(out.err.Error(), "db down") {
		t.Errorf("resolver failure leaked to the client: %v", out.err)
	}
	if !strings.Contains(out.logs, "db down") {
		t.Errorf("resolver failure must be logged, logs: %q", out.logs)
	}
	// An unbound identity on ClaimInvite is not a way past a failing lookup.
	r.unbind = map[string]bool{claimInvite: true}
	out = runOIDC(t, requiredCfg, &fakeVerifier{}, r, claimInvite, bearer(oidcToken))
	wantReject(t, out, connect.CodeUnauthenticated, server.InvalidCredentialCode)
}

// The deadline outranks the answer. For every way the lookup's context can be
// over — in time, timed out (the answer arrives after auth.resolver_timeout),
// cancelled by the caller — against every way the resolver can answer, an
// expired context is a refusal and the answer is not read: not "success", and
// not "unbound" either, which would otherwise let a ClaimInvite through
// (reviewer F1 of #290, #294). In time, each answer keeps its own class.
func TestOIDC_ResolverOutcomeMatrix(t *testing.T) {
	type verdict struct {
		allowed bool
		code    connect.Code
		errCode string
	}
	reject := verdict{code: connect.CodeUnauthenticated, errCode: server.InvalidCredentialCode}
	allow := verdict{allowed: true}
	denyUnbound := verdict{code: connect.CodePermissionDenied, errCode: server.UnboundIdentityCode}

	results := []struct {
		name string
		p    contextx.Principal
		err  error
		// inTime is what each procedure gets when the context is alive.
		inTime map[string]verdict
	}{
		{"success", boundPrincipal, nil, map[string]verdict{claimInvite: allow, otherProc: allow}},
		{"unbound", contextx.Principal{}, server.ErrUnboundIdentity, map[string]verdict{claimInvite: allow, otherProc: denyUnbound}},
		{"refused", contextx.Principal{}, fmt.Errorf("%w: account is disabled", server.ErrCredentialRefused), map[string]verdict{claimInvite: reject, otherProc: reject}},
		{"infra error", contextx.Principal{}, errors.New("db down"), map[string]verdict{claimInvite: reject, otherProc: reject}},
	}
	timings := []struct {
		name    string
		expired bool
		run     func(t *testing.T, r *fakeExternal, procedure string) oidcRun
	}{
		{"in time", false, func(t *testing.T, r *fakeExternal, procedure string) oidcRun {
			cfg := requiredCfg
			cfg.ResolverTimeout = time.Minute

			return runOIDC(t, cfg, &fakeVerifier{}, r, procedure, bearer(oidcToken))
		}},
		{"timed out", true, func(t *testing.T, r *fakeExternal, procedure string) oidcRun {
			cfg := requiredCfg
			cfg.ResolverTimeout = 10 * time.Millisecond
			r.lateAnswer = true

			return runOIDC(t, cfg, &fakeVerifier{}, r, procedure, bearer(oidcToken))
		}},
		{"cancelled", true, func(t *testing.T, r *fakeExternal, procedure string) oidcRun {
			cfg := requiredCfg
			cfg.ResolverTimeout = time.Minute
			c, cancel := context.WithCancel(context.Background())
			cancel()

			return runOIDCCtx(t, c, cfg, &fakeVerifier{}, r, procedure, bearer(oidcToken))
		}},
	}

	for _, tm := range timings {
		for _, res := range results {
			for _, procedure := range []string{claimInvite, otherProc} {
				t.Run(tm.name+"/"+res.name+"/"+procedure, func(t *testing.T) {
					r := &fakeExternal{p: res.p, err: res.err, unbind: map[string]bool{claimInvite: true}}
					out := tm.run(t, r, procedure)

					want := res.inTime[procedure]
					if tm.expired {
						want = reject
					}
					if want.allowed {
						if out.err != nil || out.nextCalls != 1 {
							t.Fatalf("err %v, handler ran %d times, want it to run once", out.err, out.nextCalls)
						}
						if res.err == nil && out.principal.Subject != boundPrincipal.Subject {
							t.Errorf("principal = %+v, want the account's", out.principal)
						}
						if res.err != nil && (!out.principal.IsAnonymous() || !out.hasExt) {
							t.Errorf("an unbound ClaimInvite carries the verified identity and no principal, got %+v (identity %v)", out.principal, out.hasExt)
						}

						return
					}
					wantReject(t, out, want.code, want.errCode)
					if out.hasExt {
						t.Error("a rejected call must not carry the external identity")
					}
				})
			}
		}
	}
}

// A resolver that honours its context answers with the context's error; that
// is the same refusal, "unbound" semantics included.
func TestOIDC_ResolverHonouringTheDeadlineIsRefused(t *testing.T) {
	cfg := requiredCfg
	cfg.ResolverTimeout = 10 * time.Millisecond
	r := &fakeExternal{lateAnswer: true, honour: true, unbind: map[string]bool{claimInvite: true}}
	out := runOIDC(t, cfg, &fakeVerifier{}, r, claimInvite, bearer(oidcToken))
	wantReject(t, out, connect.CodeUnauthenticated, server.InvalidCredentialCode)
}

// Credential precedence: with an X-API-Key the bearer is never looked at —
// not on success and not on failure (no fallback either way).
func TestOIDC_APIKeyWins(t *testing.T) {
	v, r := &fakeVerifier{}, &fakeExternal{p: boundPrincipal}
	h := bearer(oidcToken)
	h[server.APIKeyHeader] = apiKeyValue
	out := runOIDC(t, requiredCfg, v, r, otherProc, h)
	if out.err != nil || out.principal.Subject != "ci" || out.principal.Method != server.MethodAPIKey {
		t.Fatalf("api key + bearer: %+v, %v", out.principal, out.err)
	}
	if v.calls != 0 || r.calls != 0 {
		t.Errorf("a request with an API key must not reach OIDC (verifier %d, resolver %d calls)", v.calls, r.calls)
	}

	h[server.APIKeyHeader] = "xk_wrong-0123456789abcdef0123456789"
	out = runOIDC(t, requiredCfg, v, r, otherProc, h)
	wantReject(t, out, connect.CodeUnauthenticated, server.InvalidCredentialCode)
	if v.calls != 0 || r.calls != 0 {
		t.Errorf("a bad API key must not fall back to the bearer (verifier %d, resolver %d calls)", v.calls, r.calls)
	}
}

// The OIDC verifier says whose token it is: an issuer it does not know goes
// to auth.jwt; any other OIDC failure is final and never tried on auth.jwt.
func TestOIDC_DispatchByIssuer(t *testing.T) {
	cfg := requiredCfg
	cfg.JWT = config.JWT{HS256Secret: secret, Issuer: "x-monorepo"}
	legacy := token(t, []byte(secret), jwt.SigningMethodHS256, jwt.MapClaims{"sub": "svc", "iss": "x-monorepo", "exp": time.Now().Add(time.Minute).Unix()})

	// Unknown to OIDC → the legacy verifier answers.
	r := &fakeExternal{p: boundPrincipal}
	out := runOIDC(t, cfg, &fakeVerifier{}, r, otherProc, bearer(legacy))
	if out.err != nil || out.principal.Subject != "svc" || r.calls != 0 {
		t.Errorf("legacy token: %+v, %v (external resolver calls %d)", out.principal, out.err, r.calls)
	}

	// An OIDC rejection is final, even for a token auth.jwt would accept.
	out = runOIDC(t, cfg, &fakeVerifier{err: fmt.Errorf("%w: expired", oidc.ErrInvalidToken)}, r, otherProc, bearer(legacy))
	wantReject(t, out, connect.CodeUnauthenticated, server.InvalidCredentialCode)

	// Without auth.jwt an unknown issuer is simply rejected.
	out = runOIDC(t, requiredCfg, &fakeVerifier{}, r, otherProc, bearer(legacy))
	wantReject(t, out, connect.CodeUnauthenticated, server.InvalidCredentialCode)

	// Optional mode: a bad token is never anonymous.
	optional := config.Auth{Mode: config.AuthOptional, APIKeys: requiredCfg.APIKeys}
	out = runOIDC(t, optional, &fakeVerifier{err: oidc.ErrInvalidToken}, r, otherProc, bearer(oidcToken))
	wantReject(t, out, connect.CodeUnauthenticated, server.InvalidCredentialCode)
}

// Neither the token nor the verifier's detail reaches the client or the log.
func TestOIDC_RejectionLeaksNothing(t *testing.T) {
	v := &fakeVerifier{err: fmt.Errorf("%w: signature is invalid: kid secret-kid-123", oidc.ErrInvalidToken)}
	out := runOIDC(t, requiredCfg, v, &fakeExternal{}, otherProc, bearer(oidcToken))
	wantReject(t, out, connect.CodeUnauthenticated, server.InvalidCredentialCode)
	if out.err != nil && (strings.Contains(out.err.Error(), "secret-kid-123") || strings.Contains(out.err.Error(), oidcToken)) {
		t.Errorf("client message leaks: %v", out.err)
	}
	if strings.Contains(out.logs, oidcToken) {
		t.Errorf("the token is in the log: %s", out.logs)
	}
}

// Public procedures never reach OIDC, and no credential in required mode is
// still "missing" (the OIDC wiring changes neither).
func TestOIDC_PublicAndMissing(t *testing.T) {
	v := &fakeVerifier{}
	cfg := requiredCfg
	cfg.Public = []string{otherProc}
	out := runOIDC(t, cfg, v, &fakeExternal{}, otherProc, bearer(oidcToken))
	if out.err != nil || v.calls != 0 {
		t.Errorf("public procedure: err %v, verifier calls %d", out.err, v.calls)
	}
	out = runOIDC(t, requiredCfg, v, &fakeExternal{}, otherProc, nil)
	wantReject(t, out, connect.CodeUnauthenticated, server.UnauthenticatedCode)
}
