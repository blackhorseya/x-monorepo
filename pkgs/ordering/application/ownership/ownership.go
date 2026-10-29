// Package ownership decides whose orders a caller may touch. It sits beside
// usecase (not in it) because it is a policy the use cases consult, not a use case.
package ownership

import (
	"context"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// ErrPermissionDenied is a caller asking for another customer's orders.
var ErrPermissionDenied = kernel.NewError(kernel.KindPermissionDenied, "order.permission_denied", "not allowed to act for this customer")

// Policy decides whose orders a caller may touch. The principal is read from
// the context (contextx.PrincipalFrom); the admin role name is injected
// (config auth.admin_role), never hard-coded. Anonymous callers (auth off, or
// no principal on the path) are not restricted: ownership only bites once
// there is someone to own things.
type Policy struct {
	adminRole string
}

func New(adminRole string) *Policy {
	return &Policy{adminRole: adminRole}
}

// restricted returns the principal and true when the caller is a known,
// non-admin principal, i.e. someone confined to their own orders.
func (x *Policy) restricted(c context.Context) (contextx.Principal, bool) {
	p := contextx.PrincipalFrom(c)
	if p.IsAnonymous() || p.HasRole(x.adminRole) {
		return p, false
	}

	return p, true
}

// ForCreate returns the customer an order is placed for. A non-admin places
// orders for themselves only; an admin may name any customer, and defaults to
// themselves. Anonymous callers keep the existing rule: requested is used as
// is, and the domain refuses an empty one.
func (x *Policy) ForCreate(c context.Context, requested string) (string, error) {
	p := contextx.PrincipalFrom(c)
	if p.IsAnonymous() {
		return requested, nil
	}
	if _, ok := x.restricted(c); ok && requested != "" && requested != p.Subject {
		return "", ErrPermissionDenied
	}
	if requested == "" {
		return p.Subject, nil
	}

	return requested, nil
}

// ForList returns the customer filter a listing runs with. A non-admin is
// confined to their own orders (no filter means theirs, another customer is
// denied); admin and anonymous callers keep the filter they asked for, "" = any.
func (x *Policy) ForList(c context.Context, requested string) (string, error) {
	p, ok := x.restricted(c)
	if !ok {
		return requested, nil
	}
	if requested != "" && requested != p.Subject {
		return "", ErrPermissionDenied
	}

	return p.Subject, nil
}

// CanRead reports whether the caller may see an order of customerID. Callers
// that may not get "not found" from the use case, so an order's existence is
// not revealed.
func (x *Policy) CanRead(c context.Context, customerID string) bool {
	p, ok := x.restricted(c)

	return !ok || customerID == p.Subject
}

// OwnOnly returns the customer the caller is confined to, and true when they
// are: a known non-admin principal. Admin and anonymous callers are not
// confined (false). RED stub.
func (x *Policy) OwnOnly(_ context.Context) (string, bool) {
	return "", false
}
