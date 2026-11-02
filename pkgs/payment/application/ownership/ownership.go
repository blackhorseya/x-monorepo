// Package ownership decides whose payment a caller may touch. SPEC STAGE: everybody may.
package ownership

import (
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
)

// Caller is who the request is made for.
type Caller struct {
	CustomerID string
	Admin      bool
}

// Validate refuses a caller that names nobody.
func (x Caller) Validate() error { return nil }

// CanRead: the payment's own customer and an admin.
func CanRead(Caller, *payment.Payment) error { return nil }

// CanPay: only the payment's own customer pays.
func CanPay(Caller, *payment.Payment) error { return nil }
