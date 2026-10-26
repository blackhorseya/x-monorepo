package order

import "time"

// Status is the lifecycle state of an Order.
type Status uint8

const (
	StatusUnknown Status = iota
	StatusPending
	StatusCancelled
	// StatusConfirmed: inventory reserved every line (StockReserved consumed).
	// Appended after Cancelled so stored values keep their meaning; status is
	// persisted by name anyway.
	StatusConfirmed
)

func (x Status) String() string {
	switch x {
	case StatusPending:
		return "PENDING"
	case StatusCancelled:
		return "CANCELLED"
	case StatusConfirmed:
		return "CONFIRMED"
	default:
		return "UNKNOWN"
	}
}

// CanTransitionTo encodes the state machine in one place.
func (x Status) CanTransitionTo(next Status) bool {
	switch x {
	case StatusPending:
		return next == StatusConfirmed || next == StatusCancelled
	case StatusConfirmed:
		return next == StatusCancelled // the customer may still cancel; inventory releases on OrderCancelled
	default:
		return false
	}
}

// ParseStatus is the inverse of String. Repositories use it to rebuild a Status
// from its stored name; an unknown name is corrupt data, hence an internal error.
func ParseStatus(s string) (Status, error) {
	switch s {
	case StatusPending.String():
		return StatusPending, nil
	case StatusCancelled.String():
		return StatusCancelled, nil
	case StatusConfirmed.String():
		return StatusConfirmed, nil
	default:
		return StatusUnknown, ErrInvalidStatus
	}
}

// Statuses lists every status this version knows, in lifecycle order. Readers
// that report per-status figures use it to show a zero instead of nothing.
func Statuses() []Status { return []Status{StatusPending, StatusConfirmed, StatusCancelled} }

// StatusCount is how many orders sit in one status and when the oldest of them
// last changed (zero time when there are none): the saga's funnel, as the
// repository counts it. Age is the reader's to compute — the domain has no clock.
type StatusCount struct {
	status          Status
	count           int64
	oldestUpdatedAt time.Time
}

func NewStatusCount(status Status, count int64, oldestUpdatedAt time.Time) StatusCount {
	return StatusCount{status: status, count: count, oldestUpdatedAt: oldestUpdatedAt}
}

func (x StatusCount) Status() Status             { return x.status }
func (x StatusCount) Count() int64               { return x.count }
func (x StatusCount) OldestUpdatedAt() time.Time { return x.oldestUpdatedAt }
