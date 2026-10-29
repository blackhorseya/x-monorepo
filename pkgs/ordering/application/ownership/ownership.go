// Package ownership decides whose orders a caller may touch. It sits beside
// usecase (not in it) because it is a policy the use cases consult, not a use case.
package ownership

import (
	"context"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

// ErrPermissionDenied is a caller asking for another customer's orders.
var ErrPermissionDenied = kernel.NewError(kernel.KindPermissionDenied, "order.permission_denied", "not allowed to act for this customer")

// Policy decides whose orders a caller may touch. The principal is read
// from the context (contextx.PrincipalFrom); the admin role name is injected
// (config auth.admin_role), never hard-coded. Anonymous callers (auth off, or
// no principal on the path) are not restricted: ownership only bites once
// there is someone to own things.
type Policy struct {
	adminRole string
}

func New(adminRole string) *Policy {
	return &Policy{adminRole: adminRole}
}

// ForCreate returns the customer an order is placed for.
func (x *Policy) ForCreate(_ context.Context, requested string) (string, error) {
	return requested, nil // TODO(red): not enforced yet
}

// ForList returns the customer filter a listing runs with.
func (x *Policy) ForList(_ context.Context, requested string) (string, error) {
	return requested, nil // TODO(red): not enforced yet
}

// CanRead reports whether the caller may see an order of customerID.
func (x *Policy) CanRead(_ context.Context, _ string) bool {
	return true // TODO(red): not enforced yet
}
