package order

// Status is the lifecycle state of an Order.
type Status uint8

const (
	StatusUnknown Status = iota
	StatusPending
	StatusCancelled
)

func (x Status) String() string {
	switch x {
	case StatusPending:
		return "PENDING"
	case StatusCancelled:
		return "CANCELLED"
	default:
		return "UNKNOWN"
	}
}

// CanTransitionTo encodes the state machine in one place.
func (x Status) CanTransitionTo(next Status) bool {
	switch x {
	case StatusPending:
		return next == StatusCancelled
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
	default:
		return StatusUnknown, ErrInvalidStatus
	}
}
