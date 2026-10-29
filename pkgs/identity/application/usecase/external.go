package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/mapper"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/externalidentity"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/invite"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// actor is who the audit line names: the calling principal's subject, or
// "anonymous" when auth is off and nobody was identified.
func actor(c context.Context) string {
	if p := contextx.PrincipalFrom(c); !p.IsAnonymous() {
		return p.Subject
	}

	return "anonymous"
}

func toInviteOutput(v *invite.Invite) *dto.InviteOutput {
	return &dto.InviteOutput{
		ID: v.ID().String(), Email: v.Email(), AccountID: v.AccountID(),
		CreatedAt: v.CreatedAt(), ExpiresAt: v.ExpiresAt(), ClaimedAt: v.ClaimedAt(),
	}
}

// CreateInvite pre-binds an email to an account: the person signs in at the
// IdP and claims it (ClaimInvite). Valid 7 days, single use, no secret.
type CreateInvite struct {
	accounts account.Repository
	invites  invite.Repository
	ids      port.IDGenerator
	audit    port.AuditLog
	clock    port.Clock
}

func NewCreateInvite(accounts account.Repository, invites invite.Repository, ids port.IDGenerator, audit port.AuditLog, clock port.Clock) *CreateInvite {
	return &CreateInvite{accounts: accounts, invites: invites, ids: ids, audit: audit, clock: clock}
}

func (uc *CreateInvite) Execute(c context.Context, in dto.CreateInviteInput) (*dto.InviteOutput, error) {
	ctx := contextx.FromContext(c)
	if in.AccountID == "" {
		return nil, account.ErrInvalidID
	}
	if _, err := uc.accounts.FindByID(ctx, account.ID(in.AccountID)); err != nil {
		return nil, err
	}
	id, err := uc.ids.NewID(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate invite id: %w", err)
	}
	now := uc.clock.Now()
	v, err := invite.New(invite.ID(id), in.Email, in.AccountID, now)
	if err != nil {
		return nil, err
	}
	if err := uc.invites.Add(ctx, v); err != nil {
		return nil, fmt.Errorf("add invite %s: %w", id, err)
	}
	if err := uc.audit.Record(ctx, dto.AuditEvent{Actor: actor(ctx), Action: dto.AuditCreateInvite, TargetAccountID: in.AccountID, At: now}); err != nil {
		return nil, fmt.Errorf("audit create invite: %w", err)
	}

	return toInviteOutput(v), nil
}

// ClaimInvite binds a verified external identity to the account its email was
// invited for, and spends the invite. The caller has already verified the
// token; this checks the invite: pending, unexpired, email verified and equal.
//
// It runs in one transaction (decorateTx) and the invite row is locked by the
// lookup, so two subs claiming one invite at once serialize: the second finds
// it spent. After the claim only (issuer, sub) counts, never the email.
type ClaimInvite struct {
	invites  invite.Repository
	bindings externalidentity.Repository
	accounts account.Repository
	clock    port.Clock
}

func NewClaimInvite(invites invite.Repository, bindings externalidentity.Repository, accounts account.Repository, clock port.Clock) *ClaimInvite {
	return &ClaimInvite{invites: invites, bindings: bindings, accounts: accounts, clock: clock}
}

func (uc *ClaimInvite) Execute(c context.Context, in dto.ClaimInviteInput) (*dto.AccountOutput, error) {
	ctx := contextx.FromContext(c)
	email, err := invite.NormalizeEmail(in.Email)
	if err != nil {
		return nil, err
	}
	v, err := uc.invites.FindLatestByEmail(ctx, email) // locks the row until the transaction ends
	if err != nil {
		return nil, err
	}
	// Read the clock only now: a claimant that queued behind another on the
	// lock must judge expiry at the moment it holds it.
	now := uc.clock.Now()
	if err := v.Claim(in.Email, in.EmailVerified, now); err != nil {
		return nil, err
	}
	b, err := externalidentity.New(in.Issuer, in.Subject, v.AccountID(), now)
	if err != nil {
		return nil, err
	}
	// Postgres' primary key is the guarantee; this read keeps the memory
	// repositories (no rollback) from spending the invite on a lost claim.
	switch _, err := uc.bindings.Find(ctx, in.Issuer, in.Subject); {
	case err == nil:
		return nil, externalidentity.ErrDuplicate
	case !errors.Is(err, externalidentity.ErrNotFound):
		return nil, fmt.Errorf("find external identity: %w", err)
	}
	if err := uc.bindings.Add(ctx, b); err != nil {
		return nil, fmt.Errorf("bind external identity to account %s: %w", v.AccountID(), err)
	}
	if err := uc.invites.Save(ctx, v); err != nil {
		return nil, fmt.Errorf("spend invite %s: %w", v.ID(), err)
	}
	a, err := uc.accounts.FindByID(ctx, account.ID(v.AccountID()))
	if err != nil {
		return nil, err
	}

	return mapper.ToAccountOutput(a), nil
}

// RevokeSessions is "sign out everywhere": access tokens issued before now
// are refused for the account, within the resolver cache TTL. It does not
// change status, roles or API keys, so it has no last-admin guard. It cannot
// stop a holder of the IdP refresh token (see account.RevokeSessions); the
// audit line names the caller and the account, never a token.
type RevokeSessions struct {
	accounts account.Repository
	audit    port.AuditLog
	clock    port.Clock
}

func NewRevokeSessions(accounts account.Repository, audit port.AuditLog, clock port.Clock) *RevokeSessions {
	return &RevokeSessions{accounts: accounts, audit: audit, clock: clock}
}

func (uc *RevokeSessions) Execute(c context.Context, in dto.RevokeSessionsInput) (*dto.AccountOutput, error) {
	ctx := contextx.FromContext(c)
	if in.AccountID == "" {
		return nil, account.ErrInvalidID
	}
	a, err := uc.accounts.FindByID(ctx, account.ID(in.AccountID))
	if err != nil {
		return nil, err
	}
	now := uc.clock.Now()
	if err := a.RevokeSessions(now); err != nil {
		return nil, err
	}
	if err := uc.accounts.Save(ctx, a); err != nil {
		return nil, fmt.Errorf("save account %s: %w", in.AccountID, err)
	}
	if err := uc.audit.Record(ctx, dto.AuditEvent{Actor: actor(ctx), Action: dto.AuditRevokeSessions, TargetAccountID: in.AccountID, At: now}); err != nil {
		return nil, fmt.Errorf("audit revoke sessions: %w", err)
	}

	return mapper.ToAccountOutput(a), nil
}

// ResolveExternalIdentity looks up the account of a verified (issuer,
// subject): its id, roles, disabled flag and sessions_revoked_at. An unbound
// identity is externalidentity.ErrNotFound; a disabled account still resolves
// (Disabled true), because refusing it is the resolver adapter's decision.
type ResolveExternalIdentity struct {
	bindings externalidentity.Repository
	accounts account.Repository
}

func NewResolveExternalIdentity(bindings externalidentity.Repository, accounts account.Repository) *ResolveExternalIdentity {
	return &ResolveExternalIdentity{bindings: bindings, accounts: accounts}
}

func (uc *ResolveExternalIdentity) Execute(c context.Context, in dto.ResolveExternalInput) (*dto.ResolveExternalOutput, error) {
	ctx := contextx.FromContext(c)
	b, err := uc.bindings.Find(ctx, in.Issuer, in.Subject)
	if err != nil {
		return nil, err
	}
	a, err := uc.accounts.FindByID(ctx, account.ID(b.AccountID()))
	if err != nil {
		return nil, fmt.Errorf("find account %s of external identity: %w", b.AccountID(), err)
	}

	return &dto.ResolveExternalOutput{
		AccountID: a.ID().String(), Name: a.Name(), Roles: a.Roles(),
		Disabled: !a.IsActive(), SessionsRevokedAt: a.SessionsRevokedAt(),
	}, nil
}

// ProvisionExternalIdentity is auto_provision: the first sight of an identity
// of such an issuer creates an account and binds it, in one transaction. The
// account has NO role (roles are an admin's to give). The loser of a race on
// the same identity gets externalidentity.ErrDuplicate, with its account
// rolled back; the resolver re-reads.
type ProvisionExternalIdentity struct {
	accounts account.Repository
	bindings externalidentity.Repository
	ids      port.IDGenerator
	clock    port.Clock
}

func NewProvisionExternalIdentity(accounts account.Repository, bindings externalidentity.Repository, ids port.IDGenerator, clock port.Clock) *ProvisionExternalIdentity {
	return &ProvisionExternalIdentity{accounts: accounts, bindings: bindings, ids: ids, clock: clock}
}

func (uc *ProvisionExternalIdentity) Execute(c context.Context, in dto.ProvisionExternalInput) (*dto.AccountOutput, error) {
	ctx := contextx.FromContext(c)
	id, err := uc.ids.NewID(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate account id: %w", err)
	}
	now := uc.clock.Now()
	b, err := externalidentity.New(in.Issuer, in.Subject, id, now)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(in.Email)
	if name == "" {
		name = in.Subject
	}
	a, err := account.New(account.ID(id), name, nil, now)
	if err != nil {
		return nil, err
	}
	switch _, err := uc.bindings.Find(ctx, in.Issuer, in.Subject); {
	case err == nil:
		return nil, externalidentity.ErrDuplicate
	case !errors.Is(err, externalidentity.ErrNotFound):
		return nil, fmt.Errorf("find external identity: %w", err)
	}
	if err := uc.accounts.Add(ctx, a); err != nil {
		return nil, fmt.Errorf("add account %s: %w", id, err)
	}
	if err := uc.bindings.Add(ctx, b); err != nil {
		return nil, fmt.Errorf("bind external identity to account %s: %w", id, err)
	}

	return mapper.ToAccountOutput(a), nil
}
