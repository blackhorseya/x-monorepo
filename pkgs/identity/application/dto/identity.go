// Package dto is the application boundary of the identity bounded context.
// Rule: carry only what a use case needs, never mirror the proto contract.
package dto

import "time"

type AccountOutput struct {
	ID                string
	Name              string
	Roles             []string
	Status            string // ACTIVE | DISABLED
	CreatedAt         time.Time
	UpdatedAt         time.Time
	SessionsRevokedAt time.Time // zero: never revoked
}

type CreateAccountInput struct {
	Name  string
	Roles []string
}

type GetAccountInput struct {
	ID string
}

type ListAccountsInput struct {
	PageSize  int32
	PageToken string
}

type ListAccountsOutput struct {
	Accounts      []AccountOutput
	NextPageToken string
}

type DisableAccountInput struct {
	ID string
}

type EnableAccountInput struct {
	ID string
}

type AssignRolesInput struct {
	ID    string
	Roles []string
}

type APIKeyOutput struct {
	ID         string
	AccountID  string
	Name       string
	Prefix     string
	Status     string // ACTIVE | REVOKED
	CreatedAt  time.Time
	ExpiresAt  time.Time // zero: never
	RevokedAt  time.Time // zero: not revoked
	LastUsedAt time.Time // zero: never used (throttled, best-effort: can trail by about two auth.last_used_interval)
}

// CreateAPIKeyInput mints a key; ExpiresAt zero means it never expires.
type CreateAPIKeyInput struct {
	AccountID string
	Name      string
	ExpiresAt time.Time
}

// CreateAPIKeyOutput carries the plaintext Secret exactly once.
type CreateAPIKeyOutput struct {
	Key    APIKeyOutput
	Secret string
}

type ListAPIKeysInput struct {
	AccountID string
}

type ListAPIKeysOutput struct {
	Keys []APIKeyOutput
}

type RevokeAPIKeyInput struct {
	ID string
}

// ResolveAPIKeyInput is a presented X-API-Key; the output is what the
// platform's principal is built from. Failing to resolve is a domain error
// (api_key.not_found / revoked / expired, account.disabled): the adapter
// turns every one of them into "invalid credential".
type ResolveAPIKeyInput struct {
	Secret string
}

type ResolveAPIKeyOutput struct {
	AccountID string
	Name      string
	Roles     []string
	KeyID     string
	ExpiresAt time.Time // zero: never; a cache must not serve the key past it
}

// TouchAPIKeyInput is a key that just resolved; Recorded is false when its
// last_used_at was already stamped inside the window.
type TouchAPIKeyInput struct {
	KeyID string
}

type TouchAPIKeyOutput struct {
	Recorded bool
}

// WhoAmIInput has no fields: the answer is the principal in the context.
type WhoAmIInput struct{}

type WhoAmIOutput struct {
	Anonymous    bool
	Subject      string
	Name         string
	Roles        []string
	Method       string
	CredentialID string
}

// BootstrapInput is the first account and its first key, made together when
// the store holds no account at all (`worker bootstrap`).
type BootstrapInput struct {
	Name      string
	Roles     []string
	KeyName   string
	ExpiresAt time.Time // zero: never
	// AdminEmail switches bootstrap to the OIDC path: the admin account gets an
	// invite for this email instead of an API key (no secret exists to print).
	AdminEmail string
}

// BootstrapOutput: Created false means an account already existed and
// nothing was written (Account / Key / Secret are then empty). Secret is
// the plaintext, handed out exactly once.
type BootstrapOutput struct {
	Created bool
	Account AccountOutput
	Key     APIKeyOutput
	Secret  string
	// Invite is set when BootstrapInput.AdminEmail was; Key and Secret are then empty.
	Invite InviteOutput
}

// InviteOutput is an invite as the admin sees it: it carries no secret.
type InviteOutput struct {
	ID        string
	Email     string
	AccountID string
	CreatedAt time.Time
	ExpiresAt time.Time
	ClaimedAt time.Time // zero: still pending
	Status    string    // PENDING | CLAIMED | EXPIRED, judged when the output was made
}

// ExternalIdentityOutput is one (issuer, subject) bound to an account.
type ExternalIdentityOutput struct {
	Issuer    string
	Subject   string
	AccountID string
	BoundAt   time.Time
}

type ListAccountIdentitiesInput struct {
	AccountID string
}

// ListAccountIdentitiesOutput is an account's bindings and invites, newest
// first. Neither carries a token, hash or secret: invites hold none.
type ListAccountIdentitiesOutput struct {
	Identities []ExternalIdentityOutput
	Invites    []InviteOutput
}

type CreateInviteInput struct {
	AccountID string
	Email     string
}

// ClaimInviteInput is a verified external identity asking to be bound; the
// caller (the auth interceptor) has already verified the token.
type ClaimInviteInput struct {
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
}

type RevokeSessionsInput struct {
	AccountID string
}

// ResolveExternalInput is the (issuer, subject) of a verified token.
type ResolveExternalInput struct {
	Issuer  string
	Subject string
}

// ResolveExternalOutput is what a request's principal is built from. A
// disabled account still resolves (Disabled true): refusing it is the
// resolver adapter's decision.
type ResolveExternalOutput struct {
	AccountID         string
	Name              string
	Roles             []string
	Disabled          bool
	SessionsRevokedAt time.Time // zero: never revoked
}

// ProvisionExternalInput creates a role-less account for a first-seen
// identity of an auto_provision issuer. Email only names the account.
type ProvisionExternalInput struct {
	Issuer  string
	Subject string
	Email   string
}

// Audit actions.
const (
	AuditRevokeSessions = "identity.revoke_sessions"
	AuditCreateInvite   = "identity.create_invite"
)

// AuditEvent is who did what to which account, and when. It never carries a
// token or a secret.
type AuditEvent struct {
	Actor           string // the calling principal's subject; "anonymous" when auth is off
	Action          string
	TargetAccountID string
	At              time.Time
}
