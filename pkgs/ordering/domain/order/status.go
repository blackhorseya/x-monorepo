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
