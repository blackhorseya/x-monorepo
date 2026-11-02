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
// own payment_required snapshot and status — never by the setting as it is now (docs/payment.md §8):
//
//	PENDING (never confirmed)                refused, ErrNotConfirmed (payment exists only after OrderConfirmed)
//	snapshot false, CONFIRMED                PaymentUnexpected: ignored, but a defect upstream (StartPayment refuses these)
//	snapshot false, any other                PaymentIgnored
//	snapshot true, CONFIRMED                 PAID and on to COMPLETED in one step (OrderPaid, OrderCompleted)
//	snapshot true, PAID / COMPLETED          PaymentNoop (a redelivery)
//	snapshot true, CANCELLED                 refused, ErrCannotPay; payment refunds the money itself (T10)
func (x *Order) ApplyPaymentCaptured(now time.Time) (PaymentEffect, error) {
	if x.status == StatusPending || x.status == StatusUnknown {
		return PaymentIgnored, ErrNotConfirmed
	}
	if !x.paymentRequired {
		if x.status == StatusConfirmed {
			return PaymentUnexpected, nil
		}

		return PaymentIgnored, nil
	}

	switch x.status {
	case StatusConfirmed:
		// One transaction in the caller: the first version never shows an order resting at PAID (§2).
		// CONFIRMED may move to PAID and PAID to COMPLETED (Status.CanTransitionTo), and the snapshot
		// only blocks completing straight from CONFIRMED, so both steps are allowed here.
		x.markPaid(now)
		x.complete(now)

		return PaymentApplied, nil
	case StatusPaid, StatusCompleted:
		return PaymentNoop, nil
	default: // CANCELLED: never revived
		return PaymentIgnored, ErrCannotPay
	}
}

// ApplyPaymentExpired is ordering's answer to payment.v1.PaymentExpired (docs/payment.md §8):
//
//	PENDING (never confirmed)                refused, ErrNotConfirmed
//	snapshot false, any other                PaymentIgnored (the order is not waiting for a payment)
//	snapshot true, CONFIRMED                 cancelled (OrderCancelled; inventory releases the stock)
//	snapshot true, PAID / COMPLETED          refused, ErrCannotCancel: paid money is not undone by a timeout
//	snapshot true, CANCELLED                 PaymentNoop
func (x *Order) ApplyPaymentExpired(now time.Time) (PaymentEffect, error) {
	if x.status == StatusPending || x.status == StatusUnknown {
		return PaymentIgnored, ErrNotConfirmed
	}
	if !x.paymentRequired {
		return PaymentIgnored, nil
	}

	switch x.status {
	case StatusConfirmed:
		x.cancel(now) // CONFIRMED → CANCELLED is always allowed (Status.CanTransitionTo)

		return PaymentApplied, nil
	case StatusCancelled:
		return PaymentNoop, nil
	default: // PAID, COMPLETED
		return PaymentIgnored, ErrCannotCancel
	}
}
