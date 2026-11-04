package order

// Cause is why an order moved, as a fixed enumeration (#427 decision 4). It is what the history
// stores; the free text another BC attaches to a request (OrderCancellationRequested.reason) is
// never a cause, because it is untrusted and unstable.
type Cause uint8

const (
	CauseUnspecified Cause = iota
	// CauseRPCRequest: a client asked through the rpc (CreateOrder, CancelOrder, CompleteOrder);
	// who pressed the button is the actor, not the cause.
	CauseRPCRequest
	// CausePaymentCaptured: payment says the money arrived (ApplyPaymentCaptured).
	CausePaymentCaptured
	// CausePaymentExpired: payment says the checkout timed out (ApplyPaymentExpired).
	CausePaymentExpired
	// CauseStockReserved: inventory reserved every line (Confirm).
	CauseStockReserved
	// CauseCancellationRequested: another BC asked for the cancellation and its reason is not classified.
	CauseCancellationRequested
)

func (x Cause) String() string {
	switch x {
	case CauseRPCRequest:
		return "RPC_REQUEST"
	case CausePaymentCaptured:
		return "PAYMENT_CAPTURED"
	case CausePaymentExpired:
		return "PAYMENT_EXPIRED"
	case CauseStockReserved:
		return "STOCK_RESERVED"
	case CauseCancellationRequested:
		return "CANCELLATION_REQUESTED"
	default:
		return "UNSPECIFIED"
	}
}

// ParseCause is the inverse of String; anything outside the table is ErrInvalidCause.
func ParseCause(s string) (Cause, error) {
	for _, c := range []Cause{CauseUnspecified, CauseRPCRequest, CausePaymentCaptured, CausePaymentExpired, CauseStockReserved, CauseCancellationRequested} {
		if c.String() == s {
			return c, nil
		}
	}

	return CauseUnspecified, ErrInvalidCause
}

// validForCancel: the two reasons an order is cancelled by a caller (the other cause of a
// cancellation, CausePaymentExpired, is decided inside the aggregate).
func (x Cause) validForCancel() bool {
	return x == CauseRPCRequest || x == CauseCancellationRequested
}
