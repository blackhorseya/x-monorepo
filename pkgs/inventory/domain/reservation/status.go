package reservation

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

// ParseStatus is the inverse of String, for persistence.
func ParseStatus(s string) (Status, error) {
	for st, name := range statusNames {
		if name == s {
			return st, nil
		}
	}

	return 0, ErrInvalidStatus
}
