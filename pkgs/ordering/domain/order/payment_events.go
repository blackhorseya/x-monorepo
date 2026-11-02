package order

import "time"

// PaymentEffect is what a payment event did to the order (docs/payment.md §8, ordering table).
type PaymentEffect uint8

const (
	// PaymentIgnored: the order's snapshot says payment is not its business, so the event changes
	// nothing and is not an error.
	PaymentIgnored PaymentEffect = iota
	// PaymentApplied: the order moved (PaymentCaptured: CONFIRMED → PAID → COMPLETED;
	// PaymentExpired: CONFIRMED → CANCELLED).
	PaymentApplied
	// PaymentNoop: the order already is where the event would put it (a redelivery, or the
	// other side of the race got there first with the same result).
	PaymentNoop
	// PaymentUnexpected: ignored like PaymentIgnored, but it should not have been sent at all — a
	// PaymentCaptured for a CONFIRMED order whose snapshot says no payment. StartPayment refuses
	// such orders, so this is a defect somewhere upstream and the caller must count and alert it.
	PaymentUnexpected
)

func (x PaymentEffect) String() string {
	switch x {
	case PaymentIgnored:
		return "ignored"
	case PaymentApplied:
		return "applied"
	case PaymentNoop:
		return "noop"
	case PaymentUnexpected:
		return "unexpected"
	default:
		return "unknown"
	}
}

// ApplyPaymentCaptured is ordering's answer to payment.v1.PaymentCaptured, decided by the order's
// own payment_required snapshot and status — never by the setting as it is now (docs/payment.md §8).
func (x *Order) ApplyPaymentCaptured(now time.Time) (PaymentEffect, error) {
	return PaymentIgnored, nil // scaffold: RED
}

// ApplyPaymentExpired is ordering's answer to payment.v1.PaymentExpired (docs/payment.md §8).
func (x *Order) ApplyPaymentExpired(now time.Time) (PaymentEffect, error) {
	return PaymentIgnored, nil // scaffold: RED
}
