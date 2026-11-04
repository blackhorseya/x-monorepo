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
	// From / To are the status the order left and entered (From is StatusUnknown for Created);
	// Cause says why. They are what the order history records (#427): the recorder reads them off
	// the event and needs nothing else from the aggregate.
	From() Status
	To() Status
	Cause() Cause
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
	from       Status
	to         Status
	cause      Cause
}

// newBase reads the status the order is in NOW as the status it entered; from is the one it left.
func newBase(o *Order, at time.Time, from Status, cause Cause) base {
	return base{orderID: o.id, customerID: o.customerID, occurredAt: at, from: from, to: o.status, cause: cause}
}

func (x base) AggregateID() string   { return string(x.orderID) }
func (x base) OrderID() ID           { return x.orderID }
func (x base) CustomerID() string    { return x.customerID }
func (x base) OccurredAt() time.Time { return x.occurredAt }
func (x base) From() Status          { return x.from }
func (x base) To() Status            { return x.to }
func (x base) Cause() Cause          { return x.cause }

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
		base:  newBase(o, o.createdAt, StatusUnknown, CauseRPCRequest),
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

// Confirmed is raised by Confirm.
type Confirmed struct {
	base
	total moneyView
}

// NewConfirmed builds the event for o as it stands; the aggregate calls it from Confirm.
// It carries the order's actual total: Payment bills from it.
func NewConfirmed(o *Order, from Status) Confirmed {
	return Confirmed{
		base:  newBase(o, o.updatedAt, from, CauseStockReserved),
		total: moneyView{amount: o.total.Amount(), currency: o.total.Currency()},
	}
}

func (x Confirmed) TotalAmount() int64    { return x.total.amount }
func (x Confirmed) TotalCurrency() string { return x.total.currency }

// Paid is raised by MarkPaid.
type Paid struct {
	base
}

// NewPaid builds the event for o as it stands; the aggregate calls it from MarkPaid.
func NewPaid(o *Order, from Status, cause Cause) Paid {
	return Paid{base: newBase(o, o.updatedAt, from, cause)}
}

// Completed is raised by Complete.
type Completed struct {
	base
}

// NewCompleted builds the event for o as it stands; the aggregate calls it from Complete.
func NewCompleted(o *Order, from Status, cause Cause) Completed {
	return Completed{base: newBase(o, o.updatedAt, from, cause)}
}

// Cancelled is raised by Cancel.
type Cancelled struct {
	base
}

// NewCancelled builds the event for o as it stands; the aggregate calls it from Cancel.
func NewCancelled(o *Order, from Status, cause Cause) Cancelled {
	return Cancelled{base: newBase(o, o.updatedAt, from, cause)}
}

// PullEvents returns the events recorded since the last call and clears them.
// Repositories never persist them; Reconstitute starts with none.
func (x *Order) PullEvents() []Event {
	evs := x.events
	x.events = nil

	return evs
}

func (x *Order) record(e Event) { x.events = append(x.events, e) }
