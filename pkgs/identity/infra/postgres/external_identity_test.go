package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/identity"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/adapters/auth"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/adapters/rpc"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/externalidentity"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/invite"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/infra/system"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

const issuer = "https://idp.example"

// extWorld is the production wiring (identity.Provide*) over a database only
// this test uses.
type extWorld struct {
	ctx   context.Context
	db    *gorm.DB
	clock *stepClock

	accounts account.Repository
	invites  invite.Repository
	bindings externalidentity.Repository

	create    rpc.CreateInviteUC
	claim     rpc.ClaimInviteUC
	revoke    rpc.RevokeSessionsUC
	resolve   auth.ResolveExternalUC
	provision auth.ProvisionExternalUC
	disable   rpc.DisableAccountUC
}

func newExtWorld(t *testing.T) *extWorld {
	t.Helper()
	db := openDB(t, ownedDatabase(t))
	rec, err := usecasex.NewRecorder(noop.NewMeterProvider())
	if err != nil {
		t.Fatal(err)
	}
	w := &extWorld{ctx: testCtx(t), db: db, clock: &stepClock{t: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)}}
	w.accounts, w.invites, w.bindings = postgres.NewAccountRepository(db), postgres.NewInviteRepository(db), postgres.NewExternalIdentityRepository(db)
	keys, tx, audit, ids := postgres.NewAPIKeyRepository(db), postgres.NewTransactor(db), postgres.NewAuditLog(db), system.NewUUIDGenerator()
	cfg := config.Auth{AdminRole: "admin"}
	w.create = identity.ProvideCreateInvite(w.accounts, w.invites, ids, audit, w.clock, tx, rec)
	w.claim = identity.ProvideClaimInvite(w.invites, w.bindings, w.accounts, w.clock, tx, rec)
	w.revoke = identity.ProvideRevokeSessions(w.accounts, audit, w.clock, tx, rec)
	w.resolve = identity.ProvideResolveExternalIdentity(w.bindings, w.accounts, rec)
	w.provision = identity.ProvideProvisionExternalIdentity(w.accounts, w.bindings, ids, w.clock, tx, rec)
	w.disable = identity.ProvideDisableAccount(w.accounts, identity.ProvideAdminCensus(db, w.accounts, keys, w.clock), cfg, w.clock, tx, rec)

	return w
}

func (w *extWorld) newAccount(t *testing.T, id string, roles ...string) {
	t.Helper()
	a, err := account.New(account.ID(id), id, roles, w.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.accounts.Add(w.ctx, a); err != nil {
		t.Fatal(err)
	}
}

// invite stores a pending invite through the repository, not CreateInvite: the
// ClaimInvite tests must reach ClaimInvite's own behaviour even while
// CreateInvite is unimplemented.
func (w *extWorld) invite(t *testing.T, accountID, email string) {
	t.Helper()
	v, err := invite.New(invite.ID(randomName(t)), email, accountID, w.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.invites.Add(w.ctx, v); err != nil {
		t.Fatal(err)
	}
}

func (w *extWorld) count(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := w.db.WithContext(w.ctx).Raw(query, args...).Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func (w *extWorld) claimAs(sub, email string, verified bool) (*dto.AccountOutput, error) {
	return w.claim.Execute(w.ctx, dto.ClaimInviteInput{Issuer: issuer, Subject: sub, Email: email, EmailVerified: verified})
}

// race runs the calls at the same moment and returns their errors.
func race(calls ...func() error) []error {
	errs := make([]error, len(calls))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = call()
		}()
	}
	close(start)
	wg.Wait()

	return errs
}

func TestClaimInvite_RejectionsOnPostgres(t *testing.T) {
	w := newExtWorld(t)
	w.newAccount(t, "acc-1", "admin")
	w.invite(t, "acc-1", "ops@example.com")

	if _, err := w.claimAs("sub-1", "ops@example.com", false); !errors.Is(err, invite.ErrEmailUnverified) {
		t.Fatalf("unverified = %v", err)
	}
	if _, err := w.claimAs("sub-1", "other@example.com", true); !errors.Is(err, invite.ErrNotFound) {
		t.Fatalf("other email = %v", err)
	}
	if n := w.count(t, "SELECT count(*) FROM identity.external_identities"); n != 0 {
		t.Fatalf("refused claims bound %d identities", n)
	}
	// Seven days later the invite is dead.
	w.clock.advance(7 * 24 * time.Hour)
	if _, err := w.claimAs("sub-1", "ops@example.com", true); !errors.Is(err, invite.ErrExpired) {
		t.Fatalf("expired = %v", err)
	}
	// A new invite works once; the second use is refused and binds nothing.
	w.invite(t, "acc-1", "ops@example.com")
	if _, err := w.claimAs("sub-1", "ops@example.com", true); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := w.claimAs("sub-2", "ops@example.com", true); !errors.Is(err, invite.ErrAlreadyClaimed) {
		t.Fatalf("reuse = %v", err)
	}
	if n := w.count(t, "SELECT count(*) FROM identity.external_identities WHERE account_id = 'acc-1'"); n != 1 {
		t.Fatalf("bindings = %d, want 1", n)
	}
	// After the claim only the sub counts.
	out, err := w.resolve.Execute(w.ctx, dto.ResolveExternalInput{Issuer: issuer, Subject: "sub-1"})
	if err != nil || out.AccountID != "acc-1" {
		t.Fatalf("resolve = %+v, %v", out, err)
	}
}

// Two different subs claim one invite at the same moment: the row lock makes
// the second wait, see it spent, and lose.
func TestClaimInvite_TwoSubsRaceOnPostgres(t *testing.T) {
	w := newExtWorld(t)
	for round := range 15 {
		acc, email := fmt.Sprintf("acc-%d", round), fmt.Sprintf("p%d@example.com", round)
		w.newAccount(t, acc)
		w.invite(t, acc, email)

		errs := race(
			func() error { _, err := w.claimAs(fmt.Sprintf("sub-a-%d", round), email, true); return err },
			func() error { _, err := w.claimAs(fmt.Sprintf("sub-b-%d", round), email, true); return err },
		)
		won, lost := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				won++
			case errors.Is(err, invite.ErrAlreadyClaimed):
				lost++
			default:
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
		if won != 1 || lost != 1 {
			t.Fatalf("round %d: won %d lost %d (%v), want exactly one winner", round, won, lost, errs)
		}
		if n := w.count(t, "SELECT count(*) FROM identity.external_identities WHERE account_id = ?", acc); n != 1 {
			t.Fatalf("round %d: %d bindings for the account, want 1", round, n)
		}
		if n := w.count(t, "SELECT count(*) FROM identity.invites WHERE email = ? AND claimed_at IS NOT NULL", email); n != 1 {
			t.Fatalf("round %d: invite not spent exactly once", round)
		}
	}
}

// One (issuer, sub) racing for two different invites: the primary key lets one
// binding through, the loser's transaction rolls back and its invite stays pending.
func TestClaimInvite_SameSubTwoInvitesRaceOnPostgres(t *testing.T) {
	w := newExtWorld(t)
	w.newAccount(t, "acc-1")
	w.newAccount(t, "acc-2")
	w.invite(t, "acc-1", "one@example.com")
	w.invite(t, "acc-2", "two@example.com")

	errs := race(
		func() error { _, err := w.claimAs("sub-1", "one@example.com", true); return err },
		func() error { _, err := w.claimAs("sub-1", "two@example.com", true); return err },
	)
	won, dup := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, externalidentity.ErrDuplicate):
			dup++
		default:
			t.Fatalf("unexpected error %v", err)
		}
	}
	if won != 1 || dup != 1 {
		t.Fatalf("won %d duplicate %d (%v), want 1 and 1", won, dup, errs)
	}
	if n := w.count(t, "SELECT count(*) FROM identity.external_identities"); n != 1 {
		t.Fatalf("bindings = %d, want 1", n)
	}
	if n := w.count(t, "SELECT count(*) FROM identity.invites WHERE claimed_at IS NULL"); n != 1 {
		t.Fatalf("pending invites = %d, want the loser's to stay pending", n)
	}
}

func TestProvisionExternalIdentity_RaceOnPostgres(t *testing.T) {
	w := newExtWorld(t)
	in := dto.ProvisionExternalInput{Issuer: issuer, Subject: "sub-1", Email: "new@example.com"}
	errs := race(
		func() error { _, err := w.provision.Execute(w.ctx, in); return err },
		func() error { _, err := w.provision.Execute(w.ctx, in); return err },
	)
	won, dup := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, externalidentity.ErrDuplicate):
			dup++
		default:
			t.Fatalf("unexpected error %v", err)
		}
	}
	if won != 1 || dup != 1 {
		t.Fatalf("won %d duplicate %d (%v), want 1 and 1", won, dup, errs)
	}
	// The loser's account rolled back with its transaction: no orphan, and no role.
	if n := w.count(t, "SELECT count(*) FROM identity.accounts"); n != 1 {
		t.Fatalf("accounts = %d, want 1", n)
	}
	if n := w.count(t, "SELECT count(*) FROM identity.accounts WHERE roles = '[]'::jsonb"); n != 1 {
		t.Fatal("an auto_provision account must have no role")
	}
}

func TestRevokeSessions_AuditOnPostgres(t *testing.T) {
	w := newExtWorld(t)
	w.newAccount(t, "acc-1", "admin")
	admin := contextx.WithPrincipal(w.ctx, contextx.Principal{Subject: "root-1"})
	w.clock.advance(time.Hour)

	out, err := w.revoke.Execute(admin, dto.RevokeSessionsInput{AccountID: "acc-1"})
	if err != nil || !out.SessionsRevokedAt.Equal(w.clock.Now()) {
		t.Fatalf("revoke = %+v, %v", out, err)
	}
	got, _ := w.accounts.FindByID(w.ctx, "acc-1")
	if !got.SessionsRevokedAt().Equal(w.clock.Now()) || !got.IsActive() || !got.HasRole("admin") {
		t.Fatalf("persisted account = %+v", got)
	}
	if n := w.count(t, "SELECT count(*) FROM identity.audit_events WHERE actor = 'root-1' AND action = 'identity.revoke_sessions' AND target_account_id = 'acc-1' AND occurred_at = ?", w.clock.Now()); n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}
	// A revoke that fails leaves no audit row.
	if _, err := w.revoke.Execute(admin, dto.RevokeSessionsInput{AccountID: "nope"}); !errors.Is(err, account.ErrNotFound) {
		t.Fatalf("unknown = %v", err)
	}
	if n := w.count(t, "SELECT count(*) FROM identity.audit_events"); n != 1 {
		t.Fatalf("audit rows after a failed revoke = %d", n)
	}
}

// The resolver over the real tables: a database error refuses the request and
// leaves nothing behind in the cache; a disable is seen within the 30 s.
func TestExternalResolver_OnPostgres(t *testing.T) {
	w := newExtWorld(t)
	w.newAccount(t, "acc-1", "ops")
	b, err := externalidentity.New(issuer, "sub-1", "acc-1", w.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.bindings.Add(w.ctx, b); err != nil {
		t.Fatal(err)
	}
	r, err := auth.NewExternalResolver(w.resolve, w.provision, auth.StaticMapping{}, w.clock, noop.NewMeterProvider())
	if err != nil {
		t.Fatal(err)
	}
	id := auth.ExternalIdentity{Issuer: issuer, Subject: "sub-1", IssuedAt: w.clock.Now().Add(-time.Minute)}

	// The database is unusable for this request (its context is gone): refused.
	dead, cancel := context.WithCancel(w.ctx)
	cancel()
	if _, err := r.ResolveExternal(dead, id); err == nil || errors.Is(err, externalidentity.ErrNotFound) {
		t.Fatalf("db error = %v, want a refusal that is not 'unbound'", err)
	}
	// Nothing was cached: the next request reads the database and succeeds.
	if p, err := r.ResolveExternal(w.ctx, id); err != nil || p.AccountID != "acc-1" {
		t.Fatalf("after the error = %+v, %v", p, err)
	}

	// Disabled in the database: seen once the cached entry is 30 s old.
	if _, err := w.disable.Execute(w.ctx, dto.DisableAccountInput{ID: "acc-1"}); err != nil {
		t.Fatal(err)
	}
	w.clock.advance(auth.DefaultCacheTTL - time.Second)
	if _, err := r.ResolveExternal(w.ctx, id); err != nil {
		t.Fatalf("inside the ttl the cached answer stands: %v", err)
	}
	w.clock.advance(time.Second)
	if _, err := r.ResolveExternal(w.ctx, id); !errors.Is(err, account.ErrDisabled) {
		t.Fatalf("30 s after the disable = %v, want account.ErrDisabled", err)
	}
}

func TestCreateInvite_OnPostgres(t *testing.T) {
	w := newExtWorld(t)
	w.newAccount(t, "acc-1")
	admin := contextx.WithPrincipal(w.ctx, contextx.Principal{Subject: "root-1"})
	out, err := w.create.Execute(admin, dto.CreateInviteInput{AccountID: "acc-1", Email: "Ops@Example.com"})
	if err != nil || out.Email != "ops@example.com" || !out.ExpiresAt.Equal(w.clock.Now().Add(7*24*time.Hour)) {
		t.Fatalf("create = %+v, %v", out, err)
	}
	if n := w.count(t, "SELECT count(*) FROM identity.invites WHERE email = 'ops@example.com' AND claimed_at IS NULL"); n != 1 {
		t.Fatalf("invites = %d", n)
	}
	if n := w.count(t, "SELECT count(*) FROM identity.audit_events WHERE actor = 'root-1' AND action = 'identity.create_invite' AND target_account_id = 'acc-1'"); n != 1 {
		t.Fatalf("audit rows = %d", n)
	}
}
