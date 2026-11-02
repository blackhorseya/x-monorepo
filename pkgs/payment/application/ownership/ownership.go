// Package ownership decides whose payment a caller may touch. It sits beside usecase because it is
// a policy the use cases consult, not a use case.
//
// payment-rpc has no end users of its own: its only caller is apps/rpc with the service key, and
// apps/rpc says who the end user is (customer_id) and whether they are an admin (docs/payment.md
// §6). Both are therefore request data, trusted because the service key got the request this far;
// an empty customer_id is refused, never read as "anyone".
package ownership

import (
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
)

// Caller is who the request is made for.
type Caller struct {
	CustomerID string
	Admin      bool
}

// Validate refuses a caller that names nobody: an empty customer_id is never a wildcard.
func (x Caller) Validate() error {
	if x.CustomerID == "" {
		return payment.ErrEmptyCustomer
	}

	return nil
}

// CanRead: the payment's own customer and an admin. Anyone else gets ErrNotFound, the same answer
// as for an order that has no payment, so the existence of somebody else's payment is not revealed.
func CanRead(who Caller, p *payment.Payment) error {
	if err := who.Validate(); err != nil {
		return err
	}
	if who.Admin || who.CustomerID == p.CustomerID() {
		return nil
	}

	return payment.ErrNotFound
}

// CanPay: only the payment's own customer pays. An admin paying for somebody else's order is
// refused loudly (ErrPermissionDenied: the first version does not open that door); any other
// customer gets ErrNotFound as for a read.
func CanPay(who Caller, p *payment.Payment) error {
	if err := who.Validate(); err != nil {
		return err
	}
	if who.CustomerID == p.CustomerID() {
		return nil
	}
	if who.Admin {
		return payment.ErrPermissionDenied
	}

	return payment.ErrNotFound
}
