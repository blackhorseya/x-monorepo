package server

import (
	"context"
	"errors"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/oidc"
)

// UnboundIdentityCode is the x-error-code (PERMISSION_DENIED) when a token
// verifies but its (issuer, subject) is bound to no account and the procedure
// is not one an unbound identity may call (docs/identity-login.md §6).
const UnboundIdentityCode = "auth.unbound_identity"

// Errors a ExternalIdentityResolver answers with. Anything else it returns is
// an infrastructure failure (the lookup could not be made): the request is
// rejected too, but the cause is logged and never cached.
var (
	// ErrUnboundIdentity: the token is valid, no account is bound to it.
	ErrUnboundIdentity = errors.New("external identity is not bound to an account")
	// ErrCredentialRefused: the identity maps to an account that may not log in
	// (disabled, session revoked, token without iat). The client sees the same
	// rejection as a bad token.
	ErrCredentialRefused = errors.New("credential refused")
)

// TokenVerifier verifies an external OIDC bearer token (platform/oidc.Verifier).
// oidc.ErrUnknownIssuer means "not ours": the token is handed to the legacy
// auth.jwt verifier. Every other error is final.
type TokenVerifier interface {
	Verify(c context.Context, token string) (oidc.Identity, error)
}

// ExternalIdentityResolver maps a verified external identity to the principal
// of its account. The identity BC implements it (adapters/auth) and the
// composition root wires it in, like CredentialResolver, so the platform never
// imports a BC. The principal's Subject is the account id and Roles are the
// account's, never the token's. The resolver owns its cache and must not cache
// a failure. It answers with ErrUnboundIdentity, ErrCredentialRefused, or any
// other error for an infrastructure failure.
type ExternalIdentityResolver interface {
	ResolveExternal(c context.Context, id contextx.ExternalIdentity) (contextx.Principal, error)
	// AllowsUnbound reports whether an identity bound to no account may call
	// procedure (the identity BC answers true for ClaimInvite only).
	AllowsUnbound(procedure string) bool
}

// OIDCAuth is the external-login half of the auth interceptor. The zero value
// is "OIDC off": every bearer token goes to auth.jwt as before.
type OIDCAuth struct {
	Verifier TokenVerifier
	Resolver ExternalIdentityResolver
}

func (o OIDCAuth) enabled() bool { return o.Verifier != nil && o.Resolver != nil }
