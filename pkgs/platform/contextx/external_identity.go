package contextx

import (
	"context"
	"time"
)

type externalIdentityKey struct{}

// ExternalIdentity is the verified identity of an external OIDC token: what
// the issuer vouches for (iss, sub, email, email_verified, iat), after the
// auth interceptor checked signature and claims. It is NOT a principal: an
// identity not yet bound to an account has none, and only ClaimInvite may run
// with it. Handlers that need it (ClaimInvite) read it from the context and
// never from the request body.
type ExternalIdentity struct {
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
	IssuedAt      time.Time
}

// WithExternalIdentity returns a child context carrying id.
func WithExternalIdentity(c context.Context, id ExternalIdentity) context.Context {
	return context.WithValue(c, externalIdentityKey{}, id)
}

// ExternalIdentityFrom returns the verified external identity of the call, or
// false when the request did not carry an OIDC token.
func ExternalIdentityFrom(c context.Context) (ExternalIdentity, bool) {
	id, ok := c.Value(externalIdentityKey{}).(ExternalIdentity)

	return id, ok
}
