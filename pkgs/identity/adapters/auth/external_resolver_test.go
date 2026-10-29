package auth_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/adapters/auth"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/externalidentity"
)

const (
	idp   = "https://idp.example"
	other = "https://other.example"
)

// extResolve answers from a table keyed (issuer, subject) and counts calls.
type extResolve struct {
	calls int
	known map[[2]string]*dto.ResolveExternalOutput
	fail  error
}

func (u *extResolve) Execute(_ context.Context, in dto.ResolveExternalInput) (*dto.ResolveExternalOutput, error) {
	u.calls++
	if u.fail != nil {
		return nil, u.fail
	}
	if out, ok := u.known[[2]string{in.Issuer, in.Subject}]; ok {
		cp := *out
		return &cp, nil
	}

	return nil, externalidentity.ErrNotFound
}

// extProvision creates the identity in the resolve table, like the real one,
// and can be told it lost the race (ErrDuplicate) to another replica.
type extProvision struct {
	calls   int
	resolve *extResolve
	fail    error
	raced   bool
}

func (u *extProvision) Execute(_ context.Context, in dto.ProvisionExternalInput) (*dto.AccountOutput, error) {
	u.calls++
	if u.fail != nil {
		return nil, u.fail
	}
	u.resolve.known[[2]string{in.Issuer, in.Subject}] = &dto.ResolveExternalOutput{AccountID: "acc-new", Name: in.Email}
	if u.raced {
		return nil, externalidentity.ErrDuplicate
	}

	return &dto.AccountOutput{ID: "acc-new", Name: in.Email}, nil
}

type extEnv struct {
	r         *auth.ExternalResolver
	resolve   *extResolve
	provision *extProvision
	clk       *clock
	t0        time.Time
}

func newExtEnv(t *testing.T, mapping auth.StaticMapping) *extEnv {
	t.Helper()
	t0 := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	e := &extEnv{clk: &clock{t: t0}, t0: t0}
	e.resolve = &extResolve{known: map[[2]string]*dto.ResolveExternalOutput{
		{idp, "sub-1"}: {AccountID: "acc-1", Name: "ops", Roles: []string{"admin"}},
	}}
	e.provision = &extProvision{resolve: e.resolve}
	r, err := auth.NewExternalResolver(e.resolve, e.provision, mapping, e.clk, noopMeter())
	if err != nil {
		t.Fatal(err)
	}
	e.r = r

	return e
}

func (e *extEnv) id(sub string, iat time.Time) auth.ExternalIdentity {
	return auth.ExternalIdentity{Issuer: idp, Subject: sub, Email: sub + "@example.com", EmailVerified: true, IssuedAt: iat}
}

func TestExternalResolver_ResolvesAndCaches(t *testing.T) {
	e := newExtEnv(t, nil)
	c := context.Background()
	iat := e.t0.Add(-time.Minute)

	p, err := e.r.ResolveExternal(c, e.id("sub-1", iat))
	if err != nil || p.AccountID != "acc-1" || p.Name != "ops" || !slices.Equal(p.Roles, []string{"admin"}) {
		t.Fatalf("resolve = %+v, %v", p, err)
	}
	for range 5 {
		if _, err := e.r.ResolveExternal(c, e.id("sub-1", iat)); err != nil {
			t.Fatal(err)
		}
	}
	if e.resolve.calls != 1 {
		t.Fatalf("calls inside the ttl = %d, want 1", e.resolve.calls)
	}
	// A role change reaches the principal once the 30 s are over, not before.
	e.resolve.known[[2]string{idp, "sub-1"}].Roles = []string{"viewer"}
	e.clk.t = e.t0.Add(auth.DefaultCacheTTL - time.Nanosecond)
	if p, _ := e.r.ResolveExternal(c, e.id("sub-1", iat)); !slices.Equal(p.Roles, []string{"admin"}) {
		t.Fatalf("inside the ttl the cached roles still stand: %v", p.Roles)
	}
	e.clk.t = e.t0.Add(auth.DefaultCacheTTL)
	if p, _ := e.r.ResolveExternal(c, e.id("sub-1", iat)); !slices.Equal(p.Roles, []string{"viewer"}) || e.resolve.calls != 2 {
		t.Fatalf("after the ttl: roles %v calls %d", p.Roles, e.resolve.calls)
	}
	// The same sub of another issuer is another identity.
	if _, err := e.r.ResolveExternal(c, auth.ExternalIdentity{Issuer: other, Subject: "sub-1", IssuedAt: iat}); !errors.Is(err, externalidentity.ErrNotFound) {
		t.Fatalf("other issuer = %v", err)
	}
}

func TestExternalResolver_DisabledTakesEffectWithinTTL(t *testing.T) {
	e := newExtEnv(t, nil)
	c := context.Background()
	iat := e.t0.Add(-time.Minute)

	if _, err := e.r.ResolveExternal(c, e.id("sub-1", iat)); err != nil {
		t.Fatal(err)
	}
	e.resolve.known[[2]string{idp, "sub-1"}].Disabled = true // an admin disables the account
	e.clk.t = e.t0.Add(auth.DefaultCacheTTL)                 // 30 s later
	if _, err := e.r.ResolveExternal(c, e.id("sub-1", iat)); !errors.Is(err, account.ErrDisabled) {
		t.Fatalf("30 s after the disable = %v, want account.ErrDisabled", err)
	}
	// Refusal is fail-closed from the cache too.
	if _, err := e.r.ResolveExternal(c, e.id("sub-1", iat)); !errors.Is(err, account.ErrDisabled) {
		t.Fatalf("cached disabled = %v", err)
	}
	if e.resolve.calls != 2 {
		t.Fatalf("calls = %d, want 2", e.resolve.calls)
	}
}

func TestExternalResolver_SessionsRevokedAt(t *testing.T) {
	e := newExtEnv(t, nil)
	c := context.Background()
	revoked := e.t0.Add(-time.Hour)
	e.resolve.known[[2]string{idp, "sub-1"}].SessionsRevokedAt = revoked

	// iat is compared on every request, the cache-hit path included: the cache
	// key is (iss, sub), and two tokens of one user carry different iats.
	if _, err := e.r.ResolveExternal(c, e.id("sub-1", revoked.Add(-time.Second))); !errors.Is(err, account.ErrSessionRevoked) {
		t.Fatalf("old token = %v", err)
	}
	if _, err := e.r.ResolveExternal(c, e.id("sub-1", revoked.Add(time.Minute))); err != nil {
		t.Fatalf("token issued after the revocation (cache hit) = %v", err)
	}
	if _, err := e.r.ResolveExternal(c, e.id("sub-1", revoked.Add(-time.Minute))); !errors.Is(err, account.ErrSessionRevoked) {
		t.Fatalf("old token on a cache hit = %v", err)
	}
	if _, err := e.r.ResolveExternal(c, e.id("sub-1", time.Time{})); !errors.Is(err, account.ErrMissingIssuedAt) {
		t.Fatalf("token without iat = %v", err)
	}
	if e.resolve.calls != 1 {
		t.Fatalf("calls = %d, want 1 (one lookup, four token checks)", e.resolve.calls)
	}
}

func TestExternalResolver_UnboundIsRefusedAndNotCached(t *testing.T) {
	e := newExtEnv(t, nil) // no mapping configured: bound
	c := context.Background()
	iat := e.t0.Add(-time.Minute)

	for range 3 {
		if _, err := e.r.ResolveExternal(c, e.id("stranger", iat)); !errors.Is(err, externalidentity.ErrNotFound) {
			t.Fatalf("unbound = %v", err)
		}
	}
	// Not cached: once the invite is claimed the very next request resolves.
	if e.resolve.calls != 3 {
		t.Fatalf("an unbound answer must not be cached: calls = %d", e.resolve.calls)
	}
	e.resolve.known[[2]string{idp, "stranger"}] = &dto.ResolveExternalOutput{AccountID: "acc-2", Name: "new"}
	if p, err := e.r.ResolveExternal(c, e.id("stranger", iat)); err != nil || p.AccountID != "acc-2" {
		t.Fatalf("after the claim = %+v, %v", p, err)
	}
	if e.provision.calls != 0 {
		t.Fatal("bound mode must never provision")
	}
}

func TestExternalResolver_AutoProvision(t *testing.T) {
	e := newExtEnv(t, auth.StaticMapping{idp: externalidentity.MappingAutoProvision})
	c := context.Background()
	iat := e.t0.Add(-time.Minute)

	p, err := e.r.ResolveExternal(c, e.id("newcomer", iat))
	if err != nil || p.AccountID != "acc-new" || len(p.Roles) != 0 {
		t.Fatalf("auto_provision = %+v, %v", p, err)
	}
	if _, err := e.r.ResolveExternal(c, e.id("newcomer", iat)); err != nil || e.provision.calls != 1 {
		t.Fatalf("second request: %v, provisions %d", err, e.provision.calls)
	}

	// Another replica provisioned the same identity first: re-read, do not fail.
	e.provision.raced = true
	p, err = e.r.ResolveExternal(c, e.id("racer", iat))
	if err != nil || p.AccountID != "acc-new" {
		t.Fatalf("lost the provisioning race = %+v, %v", p, err)
	}
	// The mapping is per issuer: another issuer without an entry stays bound.
	if _, err := e.r.ResolveExternal(c, auth.ExternalIdentity{Issuer: other, Subject: "newcomer", IssuedAt: iat}); !errors.Is(err, externalidentity.ErrNotFound) {
		t.Fatalf("other issuer = %v", err)
	}
	// A provisioning failure refuses the request.
	e.provision.fail = errDB
	if _, err := e.r.ResolveExternal(c, e.id("unlucky", iat)); !errors.Is(err, errDB) {
		t.Fatalf("provision failure = %v", err)
	}
}

func TestExternalResolver_DBErrorRefusesAndIsNotCached(t *testing.T) {
	e := newExtEnv(t, nil)
	c := context.Background()
	iat := e.t0.Add(-time.Minute)

	e.resolve.fail = errDB
	for i := 1; i <= 2; i++ {
		_, err := e.r.ResolveExternal(c, e.id("sub-1", iat))
		if !errors.Is(err, errDB) {
			t.Fatalf("db error = %v", err)
		}
		if e.resolve.calls != i {
			t.Fatalf("an error must not be cached: calls = %d after %d", e.resolve.calls, i)
		}
	}
	// A recovered database serves the next request straight away: no poisoned entry.
	e.resolve.fail = nil
	if p, err := e.r.ResolveExternal(c, e.id("sub-1", iat)); err != nil || p.AccountID != "acc-1" {
		t.Fatalf("after recovery = %+v, %v", p, err)
	}
	// An error after a cached success is still an error once the entry expires:
	// a stale success is never served past the TTL.
	e.clk.t = e.t0.Add(auth.DefaultCacheTTL)
	e.resolve.fail = errDB
	if _, err := e.r.ResolveExternal(c, e.id("sub-1", iat)); !errors.Is(err, errDB) {
		t.Fatalf("expired entry + db down = %v", err)
	}
}

func TestExternalResolver_RefusesIncompleteIdentity(t *testing.T) {
	e := newExtEnv(t, nil)
	c := context.Background()
	iat := e.t0.Add(-time.Minute)
	if _, err := e.r.ResolveExternal(c, auth.ExternalIdentity{Subject: "sub-1", IssuedAt: iat}); !errors.Is(err, externalidentity.ErrInvalidIssuer) {
		t.Fatalf("no issuer = %v", err)
	}
	if _, err := e.r.ResolveExternal(c, auth.ExternalIdentity{Issuer: idp, IssuedAt: iat}); !errors.Is(err, externalidentity.ErrInvalidSubject) {
		t.Fatalf("no subject = %v", err)
	}
	if e.resolve.calls != 0 {
		t.Fatal("an incomplete identity must not reach the database")
	}
}
