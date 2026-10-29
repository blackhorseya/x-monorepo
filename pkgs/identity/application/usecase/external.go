package usecase

import (
	"context"
	"errors"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/externalidentity"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/invite"
)

var errNotImplemented = errors.New("not implemented")

// CreateInvite pre-binds an email to an account (RED stub).
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
	return nil, errNotImplemented
}

// ClaimInvite binds a verified external identity through an invite (RED stub).
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
	return nil, errNotImplemented
}

// RevokeSessions is "sign out everywhere" (RED stub).
type RevokeSessions struct {
	accounts account.Repository
	audit    port.AuditLog
	clock    port.Clock
}

func NewRevokeSessions(accounts account.Repository, audit port.AuditLog, clock port.Clock) *RevokeSessions {
	return &RevokeSessions{accounts: accounts, audit: audit, clock: clock}
}

func (uc *RevokeSessions) Execute(c context.Context, in dto.RevokeSessionsInput) (*dto.AccountOutput, error) {
	return nil, errNotImplemented
}

// ResolveExternalIdentity looks up the account of a verified (issuer, subject) (RED stub).
type ResolveExternalIdentity struct {
	bindings externalidentity.Repository
	accounts account.Repository
}

func NewResolveExternalIdentity(bindings externalidentity.Repository, accounts account.Repository) *ResolveExternalIdentity {
	return &ResolveExternalIdentity{bindings: bindings, accounts: accounts}
}

func (uc *ResolveExternalIdentity) Execute(c context.Context, in dto.ResolveExternalInput) (*dto.ResolveExternalOutput, error) {
	return nil, errNotImplemented
}

// ProvisionExternalIdentity creates a role-less account for an auto_provision issuer (RED stub).
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
	return nil, errNotImplemented
}
