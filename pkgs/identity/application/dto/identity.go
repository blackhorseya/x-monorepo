// Package dto is the application boundary of the identity bounded context.
// Rule: carry only what a use case needs, never mirror the proto contract.
package dto

import "time"

type AccountOutput struct {
	ID        string
	Name      string
	Roles     []string
	Status    string // ACTIVE | DISABLED
	CreatedAt time.Time
	UpdatedAt time.Time
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
	ID        string
	AccountID string
	Name      string
	Prefix    string
	Status    string // ACTIVE | REVOKED
	CreatedAt time.Time
	ExpiresAt time.Time // zero: never
	RevokedAt time.Time // zero: not revoked
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
}

// BootstrapOutput: Created false means an account already existed and
// nothing was written (Account / Key / Secret are then empty). Secret is
// the plaintext, handed out exactly once.
type BootstrapOutput struct {
	Created bool
	Account AccountOutput
	Key     APIKeyOutput
	Secret  string
}
