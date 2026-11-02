package payment

// Status is where a payment stands (docs/payment.md §3).
type Status string

const (
	// StatusPending: created, the window runs, nothing collected.
	StatusPending Status = "PENDING"
	// StatusCaptured: the provider confirmed the money, amount and currency matching.
	StatusCaptured Status = "CAPTURED"
	// StatusExpired: the window ran out and the provider reported no payment. A late capture still lands.
	StatusExpired Status = "EXPIRED"
	// StatusCancelled: the order was cancelled, or completed without this payment. A late capture still lands.
	StatusCancelled Status = "CANCELLED"
	// StatusRefunding: a full refund is owed; the provider call is in progress or waits for a retry.
	StatusRefunding Status = "REFUNDING"
	// StatusRefunded: terminal, the refund went through.
	StatusRefunded Status = "REFUNDED"
	// StatusNeedsReview: terminal, money arrived but its amount or currency does not match.
	StatusNeedsReview Status = "NEEDS_REVIEW"
)

// Statuses lists every status, in the order of docs/payment.md §3.
func Statuses() []Status {
	return []Status{StatusPending, StatusCaptured, StatusExpired, StatusCancelled, StatusRefunding, StatusRefunded, StatusNeedsReview}
}

func (x Status) String() string { return string(x) }

func (x Status) valid() bool {
	for _, s := range Statuses() {
		if x == s {
			return true
		}
	}

	return false
}

// Outcome is what Payment knows about the order's end (docs/payment.md §3 order_outcome).
type Outcome string

const (
	// OutcomeNone: no word from the order yet.
	OutcomeNone Outcome = "NONE"
	// OutcomeCancelled: OrderCancelled was received.
	OutcomeCancelled Outcome = "CANCELLED"
	// OutcomeCompletedUnpaid: OrderCompleted was received while this payment had collected nothing,
	// so the order was completed without it.
	OutcomeCompletedUnpaid Outcome = "COMPLETED_UNPAID"
)

func (x Outcome) String() string { return string(x) }

func (x Outcome) valid() bool {
	return x == OutcomeNone || x == OutcomeCancelled || x == OutcomeCompletedUnpaid
}

// Source says who reports that money arrived: the provider's webhook, or the expire sweep asking
// the provider (webhook absent). The transition is the same; only the alert differs.
type Source uint8

const (
	SourceWebhook Source = iota
	SourceReconcile
)

// Event is a fact other bounded contexts consume, raised by a transition. Publishing is the
// application's job (slice 3); the domain only says which ones the transition owes.
type Event string

const (
	EventCaptured        Event = "PaymentCaptured"
	EventExpired         Event = "PaymentExpired"
	EventRefundRequested Event = "PaymentRefundRequested"
	EventRefunded        Event = "PaymentRefunded"
)

// Alert marks an outcome an operator must look at. Alerting itself is the application's job.
type Alert string

const (
	AlertNone Alert = ""
	// AlertAmountMismatch: money arrived with another amount or currency than the payment's.
	AlertAmountMismatch Alert = "amount_mismatch"
	// AlertMissingTotal: an OrderConfirmed without a total reached an existing payment.
	AlertMissingTotal Alert = "missing_total"
	// AlertOrphanWebhook: a verified notification names a checkout no payment has opened.
	AlertOrphanWebhook Alert = "orphan_webhook"
	// AlertCaptureAfterReview: money was reported again for a payment already waiting for review.
	AlertCaptureAfterReview Alert = "capture_after_review"
)

// Effect is what one transition asks the application to do besides saving the aggregate. The zero
// value is "nothing": the event was recorded or ignored.
type Effect struct {
	publish        []Event
	alert          Alert
	cancelCheckout bool
}

// NewEffect builds an Effect; the transitions of this package are its main author, tests and the
// application's fakes use it to say what they expect or return.
func NewEffect(publish []Event, alert Alert, cancelCheckout bool) Effect {
	return Effect{publish: append([]Event(nil), publish...), alert: alert, cancelCheckout: cancelCheckout}
}

// Publish lists the events the transition raised, in order (a copy).
func (x Effect) Publish() []Event { return append([]Event(nil), x.publish...) }

// Alert is non-empty when an operator must look at this.
func (x Effect) Alert() Alert { return x.alert }

// CancelCheckout asks the application to cancel the provider's checkout, best effort: a failure must
// not block the transition (docs/payment.md T7, T7').
func (x Effect) CancelCheckout() bool { return x.cancelCheckout }
