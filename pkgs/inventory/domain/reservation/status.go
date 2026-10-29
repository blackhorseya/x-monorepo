package reservation

import "time"

// Status is the lifecycle of a Reservation.
//
//	Reserve   ─┬─ RESERVED ─┬─ Release ── RELEASED   (order cancelled: stock given back)
//	           │            ├─ Commit ─── COMMITTED  (order completed: stock sold)
//	           │            └─ Expire ─── EXPIRED ─┬─ Commit ── COMMITTED    (OrderCompleted overtaken by the sweep: sold from what is available)
//	           │                                   └─ Commit ── COMMIT_SHORT (... but the stock was taken meanwhile: sold, not covered)
//	           └─ REJECTED
//	Tombstone ─── CANCELLED   (OrderCancelled arrived before OrderCreated)
//
// EXPIRED is terminal for the sweep and for OrderCancelled, but not for
// OrderCompleted: ordering refuses to cancel a COMPLETED order, so the sale
// still has to land. COMMIT_SHORT is the anomaly where it cannot: the order is
// COMPLETED, the customer was promised stock inventory no longer has, and
// operations must settle it by hand (docs/operations.md).
//
// Every other status is terminal: a late OrderCreated finds the row and
// reserves nothing, a late OrderCancelled / OrderCompleted finds it and does nothing.
type Status int

const (
	StatusReserved Status = iota + 1
	StatusRejected
	StatusReleased
	StatusCancelled
	StatusCommitted
	StatusExpired
	StatusCommitShort
)

var statusNames = map[Status]string{
	StatusReserved:    "RESERVED",
	StatusRejected:    "REJECTED",
	StatusReleased:    "RELEASED",
	StatusCancelled:   "CANCELLED",
	StatusCommitted:   "COMMITTED",
	StatusExpired:     "EXPIRED",
	StatusCommitShort: "COMMIT_SHORT",
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
	return []Status{StatusReserved, StatusRejected, StatusReleased, StatusCommitted, StatusExpired, StatusCommitShort, StatusCancelled}
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
