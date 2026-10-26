package contextx

import "context"

type principalKey struct{}

// Principal is who an RPC runs as, put into the context by the server's auth
// interceptor and read by whoever needs it (a use case, through
// PrincipalFrom). It is identity only: what the principal may do is a
// decision of the layer that reads it. Outside an authenticated RPC — the
// consumer path, tests, auth.mode off — there is none: PrincipalFrom returns
// the zero value and IsAnonymous is true.
type Principal struct {
	// Subject is the stable identifier: the JWT `sub`, the account id of a key
	// the identity BC manages, or the name of a static auth.api_keys entry.
	Subject string
	// Name is a display name (JWT `name`), may be empty.
	Name string
	// Roles are whatever the issuer put in the JWT `roles` claim, or none.
	Roles []string
	// Method is how the principal was established: "jwt" or "api_key".
	Method string
	// CredentialID names the credential itself when it has an identity of its
	// own (the api key id of a key the identity BC manages); empty for a JWT
	// or a static key. Revoking that one key is what it is for.
	CredentialID string
}

// IsAnonymous reports whether no principal was established.
func (p Principal) IsAnonymous() bool { return p.Subject == "" }

// HasRole reports whether role is among the principal's roles.
func (p Principal) HasRole(role string) bool {
	for _, r := range p.Roles {
		if r == role {
			return true
		}
	}

	return false
}

// WithPrincipal returns a child context carrying p.
func WithPrincipal(c context.Context, p Principal) context.Context {
	return context.WithValue(c, principalKey{}, p)
}

// PrincipalFrom returns the principal carried by c, or the zero (anonymous)
// value.
func PrincipalFrom(c context.Context) Principal {
	p, _ := c.Value(principalKey{}).(Principal)
	return p
}
