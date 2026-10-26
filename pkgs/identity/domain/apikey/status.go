package apikey

// Status is the key's lifecycle: ACTIVE keys resolve (unless expired or their
// account is disabled), REVOKED is terminal.
type Status uint8

const (
	StatusActive Status = iota + 1
	StatusRevoked
)

func (s Status) String() string {
	switch s {
	case StatusActive:
		return "ACTIVE"
	case StatusRevoked:
		return "REVOKED"
	default:
		return "UNKNOWN"
	}
}

// ParseStatus is the inverse of String for persisted values.
func ParseStatus(s string) (Status, error) {
	switch s {
	case "ACTIVE":
		return StatusActive, nil
	case "REVOKED":
		return StatusRevoked, nil
	default:
		return 0, ErrInvalidStatus
	}
}
