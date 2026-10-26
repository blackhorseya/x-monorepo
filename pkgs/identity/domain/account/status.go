package account

// Status is the account's lifecycle: ACTIVE accounts resolve credentials,
// DISABLED ones refuse every credential until enabled again. Neither is terminal.
type Status uint8

const (
	StatusActive Status = iota + 1
	StatusDisabled
)

func (s Status) String() string {
	switch s {
	case StatusActive:
		return "ACTIVE"
	case StatusDisabled:
		return "DISABLED"
	default:
		return "UNKNOWN"
	}
}

// ParseStatus is the inverse of String for persisted values.
func ParseStatus(s string) (Status, error) {
	switch s {
	case "ACTIVE":
		return StatusActive, nil
	case "DISABLED":
		return StatusDisabled, nil
	default:
		return 0, ErrInvalidStatus
	}
}
