package contextx

import "context"

type principalKindKey struct{}

// WithPrincipalKind returns a child context carrying the kind of the caller.
func WithPrincipalKind(c context.Context, kind string) context.Context {
	return context.WithValue(c, principalKindKey{}, kind)
}

// PrincipalKindFrom returns the kind the server's authorization interceptor
// classified the caller as, so a use case does not classify it again. It is ""
// when there is none: a caller of no kind, a public method, auth.mode off, the
// consumer path.
func PrincipalKindFrom(c context.Context) string {
	kind, _ := c.Value(principalKindKey{}).(string)

	return kind
}
