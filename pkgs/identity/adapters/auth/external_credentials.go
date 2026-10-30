package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/contract/v1/identityv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/externalidentity"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// ExternalCredentials is the identity BC's answer to server.ExternalIdentityResolver:
// it asks the ExternalResolver (cache, use case, database) and speaks the
// platform's vocabulary back. The principal is the account's — Subject is the
// account id, Roles and Name are the account's — and nothing of the token's.
type ExternalCredentials struct{ r *ExternalResolver }

var _ server.ExternalIdentityResolver = (*ExternalCredentials)(nil)

func NewExternalCredentials(r *ExternalResolver) *ExternalCredentials {
	return &ExternalCredentials{r: r}
}

// ResolveExternal returns the account's principal. server.ErrUnboundIdentity
// is an identity with no account; server.ErrCredentialRefused an account that
// may not log in (disabled, session revoked, token without iat); anything else
// is infrastructure and passes through untouched.
func (x *ExternalCredentials) ResolveExternal(c context.Context, id contextx.ExternalIdentity) (contextx.Principal, error) {
	out, err := x.r.ResolveExternal(c, ExternalIdentity{
		Issuer: id.Issuer, Subject: id.Subject, Email: id.Email, EmailVerified: id.EmailVerified, IssuedAt: id.IssuedAt,
	})
	switch {
	case err == nil:
		return contextx.Principal{Subject: out.AccountID, Name: out.Name, Roles: out.Roles, Method: server.MethodJWT}, nil
	case errors.Is(err, externalidentity.ErrNotFound):
		return contextx.Principal{}, server.ErrUnboundIdentity
	case errors.Is(err, account.ErrDisabled), errors.Is(err, account.ErrSessionRevoked), errors.Is(err, account.ErrMissingIssuedAt):
		return contextx.Principal{}, fmt.Errorf("%w: %w", server.ErrCredentialRefused, err)
	default:
		return contextx.Principal{}, err
	}
}

// AllowsUnbound: an identity bound to no account may call ClaimInvite and
// nothing else (docs/identity-login.md §6).
func (x *ExternalCredentials) AllowsUnbound(procedure string) bool {
	return procedure == identityv1connect.IdentityServiceClaimInviteProcedure
}
