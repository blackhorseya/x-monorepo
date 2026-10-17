package reservation

// Status is the lifecycle of a Reservation.
//
//	Reserve ─┬─ RESERVED ── Release ── RELEASED
//	         └─ REJECTED
type Status int

const (
	StatusReserved Status = iota + 1
	StatusRejected
	StatusReleased
)

var statusNames = map[Status]string{
	StatusReserved: "RESERVED",
	StatusRejected: "REJECTED",
	StatusReleased: "RELEASED",
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
