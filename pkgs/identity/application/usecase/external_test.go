package usecase_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/externalidentity"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/invite"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

const idp = "https://idp.example"

type extFixture struct {
	*fixture
	invites  *memory.InviteRepository
	bindings *memory.ExternalIdentityRepository
	audit    *memory.AuditLog

	createInvite   *usecase.CreateInvite
	claimInvite    *usecase.ClaimInvite
	revokeSessions *usecase.RevokeSessions
	resolveExt     *usecase.ResolveExternalIdentity
	provisionExt   *usecase.ProvisionExternalIdentity
}

func newExtFixture() *extFixture {
	f := &extFixture{fixture: newFixture(), invites: memory.NewInviteRepository(), bindings: memory.NewExternalIdentityRepository(), audit: memory.NewAuditLog()}
	f.createInvite = usecase.NewCreateInvite(f.accounts, f.invites, f.ids, f.audit, f.clock)
	f.claimInvite = usecase.NewClaimInvite(f.invites, f.bindings, f.accounts, f.clock)
	f.revokeSessions = usecase.NewRevokeSessions(f.accounts, f.audit, f.clock)
	f.resolveExt = usecase.NewResolveExternalIdentity(f.bindings, f.accounts)
	f.provisionExt = usecase.NewProvisionExternalIdentity(f.accounts, f.bindings, f.ids, f.clock)
	return f
}

func (f *extFixture) account(t *testing.T, roles ...string) *dto.AccountOutput {
	t.Helper()
	a, err := f.createAccount.Execute(context.Background(), dto.CreateAccountInput{Name: "ops", Roles: roles})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestCreateInvite(t *testing.T) {
	f := newExtFixture()
	acc := f.account(t, "admin")
	admin := contextx.WithPrincipal(context.Background(), contextx.Principal{Subject: "root-1", Method: "api_key"})

	out, err := f.createInvite.Execute(admin, dto.CreateInviteInput{AccountID: acc.ID, Email: " Ops@Example.com "})
	if err != nil || out.Email != "ops@example.com" || out.AccountID != acc.ID || !out.ExpiresAt.Equal(f.clock.t.Add(7*24*time.Hour)) || !out.ClaimedAt.IsZero() || out.ID == "" {
		t.Fatalf("create invite = %+v, %v", out, err)
	}
	// The audit line says who, what and which account; there is no token in it.
	ev := f.audit.Events()
	if len(ev) != 1 || ev[0] != (dto.AuditEvent{Actor: "root-1", Action: dto.AuditCreateInvite, TargetAccountID: acc.ID, At: f.clock.t}) {
		t.Fatalf("audit = %+v", ev)
	}
	if _, err := f.createInvite.Execute(admin, dto.CreateInviteInput{AccountID: "nope", Email: "a@b.co"}); !errors.Is(err, account.ErrNotFound) {
		t.Fatalf("unknown account = %v", err)
	}
	if _, err := f.createInvite.Execute(admin, dto.CreateInviteInput{AccountID: acc.ID, Email: "nope"}); !errors.Is(err, invite.ErrInvalidEmail) {
		t.Fatalf("bad email = %v", err)
	}
	if _, err := f.createInvite.Execute(admin, dto.CreateInviteInput{Email: "a@b.co"}); !errors.Is(err, account.ErrInvalidID) {
		t.Fatalf("empty account = %v", err)
	}
}

func TestClaimInvite(t *testing.T) {
	c := context.Background()
	claim := func(f *extFixture, sub, email string, verified bool) (*dto.AccountOutput, error) {
		return f.claimInvite.Execute(c, dto.ClaimInviteInput{Issuer: idp, Subject: sub, Email: email, EmailVerified: verified})
	}

	t.Run("binds the subject and spends the invite", func(t *testing.T) {
		f := newExtFixture()
		acc := f.account(t, "admin")
		if _, err := f.createInvite.Execute(c, dto.CreateInviteInput{AccountID: acc.ID, Email: "ops@example.com"}); err != nil {
			t.Fatal(err)
		}
		got, err := claim(f, "sub-1", "OPS@example.com", true)
		if err != nil || got.ID != acc.ID {
			t.Fatalf("claim = %+v, %v", got, err)
		}
		// From now on only the sub counts: the email may change or be re-assigned.
		res, err := f.resolveExt.Execute(c, dto.ResolveExternalInput{Issuer: idp, Subject: "sub-1"})
		if err != nil || res.AccountID != acc.ID || !slices.Equal(res.Roles, []string{"admin"}) {
			t.Fatalf("resolve after claim = %+v, %v", res, err)
		}
		// A second person with the same email cannot use the spent invite.
		if _, err := claim(f, "sub-2", "ops@example.com", true); !errors.Is(err, invite.ErrAlreadyClaimed) {
			t.Fatalf("reuse = %v, want already claimed", err)
		}
		if _, err := f.resolveExt.Execute(c, dto.ResolveExternalInput{Issuer: idp, Subject: "sub-2"}); !errors.Is(err, externalidentity.ErrNotFound) {
			t.Fatalf("the reuse must not bind: %v", err)
		}
		// The same sub again is refused too.
		if _, err := claim(f, "sub-1", "ops@example.com", true); !errors.Is(err, invite.ErrAlreadyClaimed) {
			t.Fatalf("same sub again = %v", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		f := newExtFixture()
		acc := f.account(t)
		_, _ = f.createInvite.Execute(c, dto.CreateInviteInput{AccountID: acc.ID, Email: "ops@example.com"})
		f.clock.t = f.clock.t.Add(7 * 24 * time.Hour)
		if _, err := claim(f, "sub-1", "ops@example.com", true); !errors.Is(err, invite.ErrExpired) {
			t.Fatalf("expired = %v", err)
		}
		if _, err := f.resolveExt.Execute(c, dto.ResolveExternalInput{Issuer: idp, Subject: "sub-1"}); !errors.Is(err, externalidentity.ErrNotFound) {
			t.Fatalf("an expired claim must not bind: %v", err)
		}
	})

	t.Run("email not verified", func(t *testing.T) {
		f := newExtFixture()
		acc := f.account(t)
		_, _ = f.createInvite.Execute(c, dto.CreateInviteInput{AccountID: acc.ID, Email: "ops@example.com"})
		if _, err := claim(f, "sub-1", "ops@example.com", false); !errors.Is(err, invite.ErrEmailUnverified) {
			t.Fatalf("unverified = %v", err)
		}
		// Refused, so still claimable by the real owner.
		if _, err := claim(f, "sub-1", "ops@example.com", true); err != nil {
			t.Fatalf("a refused claim must leave the invite pending: %v", err)
		}
	})

	t.Run("no invite for the email", func(t *testing.T) {
		f := newExtFixture()
		if _, err := claim(f, "sub-1", "nobody@example.com", true); !errors.Is(err, invite.ErrNotFound) {
			t.Fatalf("no invite = %v", err)
		}
		if _, err := claim(f, "sub-1", "not-an-email", true); !errors.Is(err, invite.ErrInvalidEmail) {
			t.Fatalf("garbage email = %v", err)
		}
	})

	t.Run("a sub that is already bound cannot claim another invite", func(t *testing.T) {
		f := newExtFixture()
		a1, a2 := f.account(t), f.account(t)
		_, _ = f.createInvite.Execute(c, dto.CreateInviteInput{AccountID: a1.ID, Email: "one@example.com"})
		_, _ = f.createInvite.Execute(c, dto.CreateInviteInput{AccountID: a2.ID, Email: "two@example.com"})
		if _, err := claim(f, "sub-1", "one@example.com", true); err != nil {
			t.Fatal(err)
		}
		if _, err := claim(f, "sub-1", "two@example.com", true); !errors.Is(err, externalidentity.ErrDuplicate) {
			t.Fatalf("second binding for one sub = %v", err)
		}
		// The refused claim did not spend the second invite.
		if _, err := claim(f, "sub-9", "two@example.com", true); err != nil {
			t.Fatalf("invite two must still be pending: %v", err)
		}
	})

	t.Run("a re-issued invite replaces the expired one", func(t *testing.T) {
		f := newExtFixture()
		acc := f.account(t)
		_, _ = f.createInvite.Execute(c, dto.CreateInviteInput{AccountID: acc.ID, Email: "ops@example.com"})
		f.clock.t = f.clock.t.Add(8 * 24 * time.Hour)
		_, _ = f.createInvite.Execute(c, dto.CreateInviteInput{AccountID: acc.ID, Email: "ops@example.com"})
		if _, err := claim(f, "sub-1", "ops@example.com", true); err != nil {
			t.Fatalf("claim on the new invite: %v", err)
		}
	})
}

func TestRevokeSessions(t *testing.T) {
	f := newExtFixture()
	acc := f.account(t, "admin")
	admin := contextx.WithPrincipal(context.Background(), contextx.Principal{Subject: "root-1"})
	f.clock.t = f.clock.t.Add(time.Hour)

	out, err := f.revokeSessions.Execute(admin, dto.RevokeSessionsInput{AccountID: acc.ID})
	if err != nil || !out.SessionsRevokedAt.Equal(f.clock.t) || out.Status != "ACTIVE" || !slices.Equal(out.Roles, []string{"admin"}) {
		t.Fatalf("revoke = %+v, %v", out, err)
	}
	// Persisted, and audited as who / what / which account / when (no token).
	got, _ := f.getAccount.Execute(admin, dto.GetAccountInput{ID: acc.ID})
	if !got.SessionsRevokedAt.Equal(f.clock.t) {
		t.Fatalf("not persisted: %+v", got)
	}
	ev := f.audit.Events()
	if len(ev) != 1 || ev[0] != (dto.AuditEvent{Actor: "root-1", Action: dto.AuditRevokeSessions, TargetAccountID: acc.ID, At: f.clock.t}) {
		t.Fatalf("audit = %+v", ev)
	}
	// Auth off: no principal, and the line still says so.
	if _, err := f.revokeSessions.Execute(context.Background(), dto.RevokeSessionsInput{AccountID: acc.ID}); err != nil {
		t.Fatal(err)
	}
	if ev := f.audit.Events(); len(ev) != 2 || ev[1].Actor != "anonymous" {
		t.Fatalf("anonymous audit = %+v", ev)
	}
	// A failed revoke leaves no audit line.
	if _, err := f.revokeSessions.Execute(admin, dto.RevokeSessionsInput{AccountID: "nope"}); !errors.Is(err, account.ErrNotFound) {
		t.Fatalf("unknown = %v", err)
	}
	if _, err := f.revokeSessions.Execute(admin, dto.RevokeSessionsInput{}); !errors.Is(err, account.ErrInvalidID) {
		t.Fatalf("empty = %v", err)
	}
	if len(f.audit.Events()) != 2 {
		t.Fatal("a failed revoke must not be audited")
	}
}

func TestResolveExternalIdentity(t *testing.T) {
	f := newExtFixture()
	c := context.Background()
	acc := f.account(t, "ops")
	b, err := externalidentity.New(idp, "sub-1", acc.ID, f.clock.t)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.bindings.Add(c, b); err != nil {
		t.Fatal(err)
	}
	out, err := f.resolveExt.Execute(c, dto.ResolveExternalInput{Issuer: idp, Subject: "sub-1"})
	if err != nil || out.AccountID != acc.ID || out.Name != "ops" || out.Disabled || !out.SessionsRevokedAt.IsZero() {
		t.Fatalf("resolve = %+v, %v", out, err)
	}
	// Another issuer's identical sub is a different identity.
	if _, err := f.resolveExt.Execute(c, dto.ResolveExternalInput{Issuer: "https://other.example", Subject: "sub-1"}); !errors.Is(err, externalidentity.ErrNotFound) {
		t.Fatalf("other issuer = %v", err)
	}
	// The state a request needs is reported, not hidden.
	_, _ = f.disableAccount.Execute(c, dto.DisableAccountInput{ID: acc.ID})
	f.clock.t = f.clock.t.Add(time.Minute)
	_, _ = f.revokeSessions.Execute(c, dto.RevokeSessionsInput{AccountID: acc.ID})
	out, err = f.resolveExt.Execute(c, dto.ResolveExternalInput{Issuer: idp, Subject: "sub-1"})
	if err != nil || !out.Disabled || !out.SessionsRevokedAt.Equal(f.clock.t) {
		t.Fatalf("resolve disabled/revoked = %+v, %v", out, err)
	}
}

func TestProvisionExternalIdentity(t *testing.T) {
	f := newExtFixture()
	c := context.Background()

	out, err := f.provisionExt.Execute(c, dto.ProvisionExternalInput{Issuer: idp, Subject: "sub-1", Email: "new@example.com"})
	// auto_provision never grants a role: those are an admin's to give.
	if err != nil || len(out.Roles) != 0 || out.Status != "ACTIVE" || out.Name != "new@example.com" {
		t.Fatalf("provision = %+v, %v", out, err)
	}
	res, err := f.resolveExt.Execute(c, dto.ResolveExternalInput{Issuer: idp, Subject: "sub-1"})
	if err != nil || res.AccountID != out.ID || len(res.Roles) != 0 {
		t.Fatalf("resolve provisioned = %+v, %v", res, err)
	}
	// No email: the account is named after the subject.
	out, err = f.provisionExt.Execute(c, dto.ProvisionExternalInput{Issuer: idp, Subject: "sub-2"})
	if err != nil || out.Name != "sub-2" {
		t.Fatalf("provision without email = %+v, %v", out, err)
	}
	// The same identity twice: the second is a duplicate, never a second account.
	if _, err := f.provisionExt.Execute(c, dto.ProvisionExternalInput{Issuer: idp, Subject: "sub-1"}); !errors.Is(err, externalidentity.ErrDuplicate) {
		t.Fatalf("second provision = %v", err)
	}
	if _, err := f.provisionExt.Execute(c, dto.ProvisionExternalInput{Issuer: "", Subject: "s"}); !errors.Is(err, externalidentity.ErrInvalidIssuer) {
		t.Fatalf("empty issuer = %v", err)
	}
	f.ids.err = errors.New("entropy exhausted")
	if _, err := f.provisionExt.Execute(c, dto.ProvisionExternalInput{Issuer: idp, Subject: "sub-3"}); err == nil {
		t.Fatal("id failure must fail the provision")
	}
}

func TestBootstrapAdminEmail(t *testing.T) {
	f := newExtFixture()
	c := context.Background()
	boot := usecase.NewBootstrap(f.accounts, f.keys, f.invites, f.ids, f.secrets, f.clock)

	out, err := boot.Execute(c, dto.BootstrapInput{Name: "root", Roles: []string{"admin"}, AdminEmail: "Admin@Example.com"})
	if err != nil || !out.Created || out.Account.Name != "root" || !slices.Equal(out.Account.Roles, []string{"admin"}) {
		t.Fatalf("bootstrap = %+v, %v", out, err)
	}
	// An invite instead of a key: there is no secret anywhere in the result.
	if out.Secret != "" || out.Key.ID != "" || out.Invite.Email != "admin@example.com" || out.Invite.AccountID != out.Account.ID || !out.Invite.ExpiresAt.Equal(f.clock.t.Add(7*24*time.Hour)) {
		t.Fatalf("invite result = %+v", out)
	}
	if keys, _ := f.listKeys.Execute(c, dto.ListAPIKeysInput{AccountID: out.Account.ID}); len(keys.Keys) != 0 {
		t.Fatalf("admin-email must not mint a key: %+v", keys)
	}
	// The invite is claimable by that email.
	if _, err := f.claimInvite.Execute(c, dto.ClaimInviteInput{Issuer: idp, Subject: "sub-1", Email: "admin@example.com", EmailVerified: true}); err != nil {
		t.Fatalf("claim the bootstrap invite: %v", err)
	}
	// Re-running on a non-empty store is a no-op.
	again, err := boot.Execute(c, dto.BootstrapInput{Name: "root", Roles: []string{"admin"}, AdminEmail: "admin@example.com"})
	if err != nil || again.Created || again.Invite.ID != "" {
		t.Fatalf("second bootstrap = %+v, %v", again, err)
	}
	if page, _, _ := f.accounts.List(c, account.ListParams{Limit: 10}); len(page) != 1 {
		t.Fatalf("accounts = %d, want 1", len(page))
	}

	// A bad email writes nothing.
	g := newExtFixture()
	if _, err := usecase.NewBootstrap(g.accounts, g.keys, g.invites, g.ids, g.secrets, g.clock).Execute(c, dto.BootstrapInput{Name: "root", Roles: []string{"admin"}, AdminEmail: "nope"}); !errors.Is(err, invite.ErrInvalidEmail) {
		t.Fatalf("bad email = %v", err)
	}
	if page, _, _ := g.accounts.List(c, account.ListParams{Limit: 10}); len(page) != 0 {
		t.Fatal("a bad admin email must leave the store empty")
	}
}
