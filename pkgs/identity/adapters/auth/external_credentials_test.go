package auth_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/adapters/auth"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/contract/v1/identityv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

func extIdentity(sub string, iat time.Time) contextx.ExternalIdentity {
	return contextx.ExternalIdentity{Issuer: idp, Subject: sub, Email: sub + "@example.com", EmailVerified: true, IssuedAt: iat}
}

// The principal is the account's, nothing of the token's: Subject is the
// account id, Roles and Name are the account's, Method is jwt.
func TestExternalCredentials_PrincipalIsTheAccount(t *testing.T) {
	e := newExtEnv(t, nil)
	x := auth.NewExternalCredentials(e.r)

	p, err := x.ResolveExternal(context.Background(), extIdentity("sub-1", e.t0.Add(-time.Minute)))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if p.Subject != "acc-1" || p.Name != "ops" || p.Method != server.MethodJWT || !slices.Equal(p.Roles, []string{"admin"}) || p.CredentialID != "" {
		t.Fatalf("principal = %+v", p)
	}
}

// The three ways to be refused, told apart by what the interceptor does with
// them.
func TestExternalCredentials_ErrorClasses(t *testing.T) {
	e := newExtEnv(t, nil)
	x := auth.NewExternalCredentials(e.r)
	c := context.Background()
	iat := e.t0.Add(-time.Minute)

	// Unbound: the one a ClaimInvite caller is.
	if _, err := x.ResolveExternal(c, extIdentity("stranger", iat)); !errors.Is(err, server.ErrUnboundIdentity) {
		t.Errorf("unbound = %v, want ErrUnboundIdentity", err)
	}

	// Disabled, revoked, no iat: refused, not unbound.
	e.resolve.known[[2]string{idp, "sub-1"}].Disabled = true
	if _, err := x.ResolveExternal(c, extIdentity("sub-1", iat)); !errors.Is(err, server.ErrCredentialRefused) || errors.Is(err, server.ErrUnboundIdentity) {
		t.Errorf("disabled = %v, want ErrCredentialRefused", err)
	}
	e.resolve.known[[2]string{idp, "sub-1"}].Disabled = false
	e.clk.t = e.t0.Add(auth.DefaultCacheTTL)
	e.resolve.known[[2]string{idp, "sub-1"}].SessionsRevokedAt = e.t0
	if _, err := x.ResolveExternal(c, extIdentity("sub-1", e.t0.Add(-time.Second))); !errors.Is(err, server.ErrCredentialRefused) {
		t.Errorf("iat before sessions_revoked_at = %v, want ErrCredentialRefused", err)
	}
	if _, err := x.ResolveExternal(c, extIdentity("sub-1", time.Time{})); !errors.Is(err, server.ErrCredentialRefused) {
		t.Errorf("no iat = %v, want ErrCredentialRefused", err)
	}

	// A database error is none of those: the interceptor logs it as infrastructure.
	e.resolve.fail = errDB
	e.clk.t = e.t0.Add(2 * auth.DefaultCacheTTL)
	_, err := x.ResolveExternal(c, extIdentity("sub-1", e.t0.Add(time.Second)))
	if !errors.Is(err, errDB) || errors.Is(err, server.ErrUnboundIdentity) || errors.Is(err, server.ErrCredentialRefused) {
		t.Errorf("db error = %v, want the database error itself", err)
	}
}

// Only ClaimInvite is open to an identity bound to nothing.
func TestExternalCredentials_AllowsUnboundOnlyClaimInvite(t *testing.T) {
	x := auth.NewExternalCredentials(newExtEnv(t, nil).r)
	if !x.AllowsUnbound(identityv1connect.IdentityServiceClaimInviteProcedure) {
		t.Error("ClaimInvite must be open to an unbound identity")
	}
	for _, p := range []string{
		identityv1connect.IdentityServiceWhoAmIProcedure,
		identityv1connect.IdentityServiceCreateAccountProcedure,
		identityv1connect.IdentityServiceCreateInviteProcedure,
		"/ordering.v1.OrderService/CreateOrder",
		"",
	} {
		if x.AllowsUnbound(p) {
			t.Errorf("%q must not be open to an unbound identity", p)
		}
	}
}
