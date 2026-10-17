package reservation

import (
	"slices"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

// Event is a domain event of the Reservation aggregate. The events adapter
// maps each one to the wire contract (inventory.v1 or, for a request to
// another BC, that BC's contract).
type Event interface {
	kernel.Event
	OrderID() string
}

// base is what every reservation event carries; concrete events embed it.
type base struct {
	orderID    string
	occurredAt time.Time
}

func newBase(r *Reservation, at time.Time) base {
	return base{orderID: r.orderID, occurredAt: at}
}

func (x base) AggregateID() string   { return x.orderID }
func (x base) OrderID() string       { return x.orderID }
func (x base) OccurredAt() time.Time { return x.occurredAt }

// Reserved is raised when every line was claimed.
type Reserved struct {
	base
	lines []Line
}

func NewReserved(r *Reservation) Reserved {
	return Reserved{base: newBase(r, r.updatedAt), lines: slices.Clone(r.lines)}
}

func (x Reserved) Lines() []Line { return slices.Clone(x.lines) }

// Rejected is raised when at least one line could not be covered. Nothing was
// reserved; the order should be cancelled.
type Rejected struct {
	base
	shortfall []string
}

func NewRejected(r *Reservation) Rejected {
	return Rejected{base: newBase(r, r.updatedAt), shortfall: slices.Clone(r.shortfall)}
}

// Shortfall lists the SKUs that were short, in line order.
func (x Rejected) Shortfall() []string { return slices.Clone(x.shortfall) }

// Released is raised when a RESERVED reservation is given back.
type Released struct {
	base
}

func NewReleased(r *Reservation) Released {
	return Released{base: newBase(r, r.updatedAt)}
}

// PullEvents returns the events raised since the last pull and clears them.
// Use cases call it inside the transaction that saves the aggregate.
func (x *Reservation) PullEvents() []Event {
	evs := x.events
	x.events = nil

	return evs
}

func (x *Reservation) record(e Event) { x.events = append(x.events, e) }
