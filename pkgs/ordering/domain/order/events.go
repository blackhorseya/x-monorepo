package order

import (
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

// Event is a domain event raised by the Order aggregate: kernel.Event plus
// the typed accessors of this BC. Events are recorded on the aggregate as
// behaviour runs and collected once by PullEvents, so the use case can hand
// them to the publisher inside the same transaction as Save. Integration
// (wire) encoding is the events adapter's job, not the domain's.
type Event interface {
	kernel.Event
	OrderID() ID
	CustomerID() string
}

// CreatedItem is the (sku, quantity) pair of a line item as seen by Created:
// enough for inventory to reserve, nothing about price.
type CreatedItem struct {
	sku      string
	quantity int32
}

func NewCreatedItem(sku string, quantity int32) CreatedItem {
	return CreatedItem{sku: sku, quantity: quantity}
}

func (x CreatedItem) SKU() string     { return x.sku }
func (x CreatedItem) Quantity() int32 { return x.quantity }

// base is what every event shares; concrete events embed it (composition,
// not a hierarchy) so the Event interface is satisfied in one place.
type base struct {
	orderID    ID
	customerID string
	occurredAt time.Time
}

func newBase(o *Order, at time.Time) base {
	return base{orderID: o.id, customerID: o.customerID, occurredAt: at}
}

func (x base) AggregateID() string   { return string(x.orderID) }
func (x base) OrderID() ID           { return x.orderID }
func (x base) CustomerID() string    { return x.customerID }
func (x base) OccurredAt() time.Time { return x.occurredAt }

// Created is raised by New.
type Created struct {
	base
	items []CreatedItem
	total moneyView
}

// NewCreated builds the event for o as it stands; the aggregate calls it from New.
func NewCreated(o *Order) Created {
	items := make([]CreatedItem, 0, len(o.items))
	for _, it := range o.items {
		items = append(items, NewCreatedItem(it.SKU(), it.Quantity()))
	}

	return Created{
		base:  newBase(o, o.createdAt),
		items: items,
		total: moneyView{amount: o.total.Amount(), currency: o.total.Currency()},
	}
}

func (x Created) Items() []CreatedItem  { return append([]CreatedItem(nil), x.items...) }
func (x Created) TotalAmount() int64    { return x.total.amount }
func (x Created) TotalCurrency() string { return x.total.currency }

// moneyView is the plain (amount, currency) pair an event carries; events are
// values that outlive the aggregate, so they do not hold kernel.Money methods.
type moneyView struct {
	amount   int64
	currency string
}

// Cancelled is raised by Cancel.
type Cancelled struct {
	base
}

// NewCancelled builds the event for o as it stands; the aggregate calls it from Cancel.
func NewCancelled(o *Order) Cancelled {
	return Cancelled{base: newBase(o, o.updatedAt)}
}

// PullEvents returns the events recorded since the last call and clears them.
// Repositories never persist them; Reconstitute starts with none.
func (x *Order) PullEvents() []Event {
	evs := x.events
	x.events = nil

	return evs
}

func (x *Order) record(e Event) { x.events = append(x.events, e) }
