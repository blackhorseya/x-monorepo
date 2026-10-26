// Package reservation is the Reservation aggregate of the inventory bounded
// context: what one order claimed from stock, and whether it succeeded. It
// imports the sibling stock aggregate and the shared kernel, nothing else.
package reservation

import (
	"slices"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/stock"
)

// Reservation is the aggregate root, one per order (the order id is its identity).
type Reservation struct {
	orderID   string
	lines     []Line
	status    Status
	shortfall []string // REJECTED only
	createdAt time.Time
	updatedAt time.Time
	events    []Event // raised, not yet pulled (see events.go)
}

// Params are the inputs of Reserve.
type Params struct {
	OrderID string
	Lines   []Line
	Now     time.Time
}

// Reserve claims every line from stocks (keyed by SKU; a SKU absent from the
// map counts as zero available) — all or nothing. On success every touched
// Stock has the quantity reserved and the result is RESERVED. On shortfall no
// Stock is modified and the result is REJECTED, naming the SKUs that were
// short; it is still a valid reservation to persist, so a redelivered
// OrderCreated finds it and does nothing. Either way one event is raised.
func Reserve(p Params, stocks map[string]*stock.Stock) (*Reservation, error) {
	if p.OrderID == "" {
		return nil, ErrInvalidOrderID
	}
	if len(p.Lines) == 0 {
		return nil, ErrNoLines
	}

	r := &Reservation{
		orderID:   p.OrderID,
		lines:     slices.Clone(p.Lines),
		createdAt: p.Now,
		updatedAt: p.Now,
	}

	// An order may list a SKU twice: the stock must cover the sum.
	need, order := demand(r.lines)
	if short := stock.ReserveAll(stocks, order, need, p.Now); len(short) > 0 {
		r.status = StatusRejected
		r.shortfall = short
		r.record(NewRejected(r))

		return r, nil
	}

	r.status = StatusReserved
	r.record(NewReserved(r))

	return r, nil
}

// Tombstone records that the order was cancelled before inventory saw it
// created: topics carry no ordering between them, so OrderCancelled can
// overtake OrderCreated. Nothing was reserved, so there is nothing to release
// and no event to raise; the late OrderCreated finds this row and reserves
// nothing. It is the only Reservation without lines.
func Tombstone(orderID string, now time.Time) (*Reservation, error) {
	if orderID == "" {
		return nil, ErrInvalidOrderID
	}

	return &Reservation{orderID: orderID, status: StatusCancelled, createdAt: now, updatedAt: now}, nil
}

// ReconstituteParams carry persisted state back into the aggregate, as stored.
type ReconstituteParams struct {
	OrderID   string
	Lines     []Line
	Status    Status
	Shortfall []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Reconstitute rebuilds a Reservation from persistence. Only repositories should call it.
func Reconstitute(p ReconstituteParams) (*Reservation, error) {
	if p.OrderID == "" {
		return nil, ErrInvalidOrderID
	}
	if len(p.Lines) == 0 && p.Status != StatusCancelled {
		return nil, ErrNoLines
	}
	if p.Status.String() == "UNKNOWN" {
		return nil, ErrInvalidStatus
	}

	return &Reservation{
		orderID:   p.OrderID,
		lines:     slices.Clone(p.Lines),
		status:    p.Status,
		shortfall: slices.Clone(p.Shortfall),
		createdAt: p.CreatedAt,
		updatedAt: p.UpdatedAt,
	}, nil
}

func (x *Reservation) OrderID() string      { return x.orderID }
func (x *Reservation) Lines() []Line        { return slices.Clone(x.lines) }
func (x *Reservation) Status() Status       { return x.status }
func (x *Reservation) Shortfall() []string  { return slices.Clone(x.shortfall) }
func (x *Reservation) CreatedAt() time.Time { return x.createdAt }
func (x *Reservation) UpdatedAt() time.Time { return x.updatedAt }

// Release gives a RESERVED reservation back to stocks (keyed by SKU; every
// reserved SKU must be present). Rejected and released reservations hold
// nothing, so releasing them is an error the caller can treat as final.
func (x *Reservation) Release(stocks map[string]*stock.Stock, now time.Time) error {
	if x.status != StatusReserved {
		return ErrNotReserved
	}

	need, order := demand(x.lines)
	for _, sku := range order {
		if _, ok := stocks[sku]; !ok {
			return ErrStockMissing
		}
	}
	for _, sku := range order {
		if err := stocks[sku].Release(need[sku], now); err != nil {
			return err
		}
	}

	x.status = StatusReleased
	x.updatedAt = now
	x.record(NewReleased(x))

	return nil
}

// Commit turns a RESERVED reservation into a sale: every reserved quantity
// leaves its Stock's on-hand (order completed). Same preconditions as Release;
// a reservation already released or committed refuses with ErrNotReserved,
// which the caller can treat as final.
func (x *Reservation) Commit(stocks map[string]*stock.Stock, now time.Time) error {
	if x.status != StatusReserved {
		return ErrNotReserved
	}

	need, order := demand(x.lines)
	for _, sku := range order {
		if _, ok := stocks[sku]; !ok {
			return ErrStockMissing
		}
	}
	for _, sku := range order {
		if err := stocks[sku].Commit(need[sku], now); err != nil {
			return err
		}
	}

	x.status = StatusCommitted
	x.updatedAt = now
	x.record(NewCommitted(x))

	return nil
}

// demand sums quantities per SKU and keeps first-seen order (maps iterate randomly).
func demand(lines []Line) (map[string]int32, []string) {
	need := make(map[string]int32, len(lines))
	order := make([]string, 0, len(lines))
	for _, l := range lines {
		if _, seen := need[l.sku]; !seen {
			order = append(order, l.sku)
		}
		need[l.sku] += l.quantity
	}

	return need, order
}
