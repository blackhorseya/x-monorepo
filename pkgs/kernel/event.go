package kernel

import "time"

// Event is what every bounded context's domain events have in common: which
// aggregate raised it and when (domain time). BCs declare their own event
// interface embedding this one and add typed accessors; infrastructure that
// handles events generically (outbox keys, metadata) needs only this.
type Event interface {
	// AggregateID keys the event for ordering: all events of one aggregate
	// are delivered in the order they were raised.
	AggregateID() string
	// OccurredAt is the domain time of the change (port.Clock), not wall time.
	OccurredAt() time.Time
}
