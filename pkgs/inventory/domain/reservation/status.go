package reservation

import "time"

// Status is the lifecycle of a Reservation.
//
//	Reserve   ─┬─ RESERVED ── Release ── RELEASED
//	           └─ REJECTED
//	Tombstone ─── CANCELLED   (OrderCancelled arrived before OrderCreated)
//
// Every status but RESERVED is terminal: a late OrderCreated finds the row and
// reserves nothing, a late OrderCancelled finds it and releases nothing.
type Status int

const (
	StatusReserved Status = iota + 1
	StatusRejected
	StatusReleased
	StatusCancelled
)

var statusNames = map[Status]string{
	StatusReserved:  "RESERVED",
	StatusRejected:  "REJECTED",
	StatusReleased:  "RELEASED",
	StatusCancelled: "CANCELLED",
}

func (x Status) String() string {
	if s, ok := statusNames[x]; ok {
		return s
	}

	return "UNKNOWN"
}

// Statuses lists every status this version knows, in lifecycle order. Readers
// that report per-status figures use it to show a zero instead of nothing.
func Statuses() []Status {
	return []Status{StatusReserved, StatusRejected, StatusReleased, StatusCancelled}
}

// StatusCount is how many reservations sit in one status and when the oldest
// of them last changed (zero time when none): the saga's funnel as the
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

// ParseStatus is the inverse of String, for persistence.
func ParseStatus(s string) (Status, error) {
	for st, name := range statusNames {
		if name == s {
			return st, nil
		}
	}

	return 0, ErrInvalidStatus
}
