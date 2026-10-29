// Package usecase orchestrates domain objects. Business rules stay in the
// domain; cross-cutting concerns are usecasex decorators.
package usecase

import (
	"context"
	"errors"
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/mapper"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/stock"
	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// ReserveStock reacts to an order being created: it claims every line from
// stock, all or nothing, and records the outcome as a Reservation. A shortfall
// is an outcome (REJECTED), not an error — the Rejected event asks ordering to
// cancel the order. Idempotent per order: a second delivery of the same
// OrderCreated finds the reservation and returns it unchanged.
type ReserveStock struct {
	reservations reservation.Repository
	stocks       stock.Repository
	events       port.EventPublisher
	clock        port.Clock
}

func NewReserveStock(reservations reservation.Repository, stocks stock.Repository, events port.EventPublisher, clock port.Clock) *ReserveStock {
	return &ReserveStock{reservations: reservations, stocks: stocks, events: events, clock: clock}
}

func (uc *ReserveStock) Execute(c context.Context, in dto.ReserveStockInput) (*dto.ReservationOutput, error) {
	ctx := contextx.FromContext(c)

	if in.OrderID == "" {
		return nil, reservation.ErrInvalidOrderID
	}
	lines := make([]reservation.Line, 0, len(in.Lines))
	for _, l := range in.Lines {
		line, err := reservation.NewLine(l.SKU, l.Quantity)
		if err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}

	existing, err := uc.reservations.FindByOrderID(ctx, in.OrderID)
	switch {
	case err == nil:
		return mapper.ToReservationOutput(existing), nil // already handled (redelivery)
	case !errors.Is(err, reservation.ErrNotFound):
		return nil, fmt.Errorf("find reservation: %w", err)
	}

	// Unknown SKUs are left out: the domain counts them as zero available.
	stocks, err := loadStocks(ctx, uc.stocks, lines)
	if err != nil {
		return nil, err
	}

	r, err := reservation.Reserve(reservation.Params{OrderID: in.OrderID, Lines: lines, Now: uc.clock.Now()}, stocks)
	if err != nil {
		return nil, err
	}
	if r.Status() == reservation.StatusReserved {
		if err = saveStocks(ctx, uc.stocks, stocks); err != nil {
			return nil, err
		}
	}
	if err = uc.reservations.Add(ctx, r); err != nil {
		if errors.Is(err, reservation.ErrDuplicate) {
			// Lost the insert to a redelivery or to ReleaseStock's tombstone:
			// retry lands on the "already handled" branch above.
			return nil, fmt.Errorf("add reservation: %w", kernel.ErrConcurrentUpdate)
		}

		return nil, fmt.Errorf("add reservation: %w", err)
	}
	if err = uc.events.Publish(ctx, r.PullEvents()...); err != nil {
		return nil, err
	}

	return mapper.ToReservationOutput(r), nil
}

// loadStocks fetches the Stock of every distinct SKU in lines; SKUs that were
// never stocked are left out of the map.
func loadStocks(c context.Context, repo stock.Repository, lines []reservation.Line) (map[string]*stock.Stock, error) {
	stocks := make(map[string]*stock.Stock, len(lines))
	for _, l := range lines {
		if _, ok := stocks[l.SKU()]; ok {
			continue
		}
		s, err := repo.FindBySKU(c, stock.SKU(l.SKU()))
		switch {
		case err == nil:
			stocks[l.SKU()] = s
		case errors.Is(err, stock.ErrNotFound):
			continue
		default:
			return nil, fmt.Errorf("find stock %s: %w", l.SKU(), err)
		}
	}

	return stocks, nil
}

func saveStocks(c context.Context, repo stock.Repository, stocks map[string]*stock.Stock) error {
	for sku, s := range stocks {
		if err := repo.Save(c, s); err != nil {
			return fmt.Errorf("save stock %s: %w", sku, err)
		}
	}

	return nil
}

// staleIfMoved classifies a failed domain call on r. The stock was loaded
// after r was read, so when another transaction moved r in between (expired,
// released, committed), the stock no longer matches the status r was read in
// and the aggregate fails with a domain error (e.g. commit_exceeds_reserved)
// that is really a lost race. If r moved since it was loaded the error is
// reservation.ErrStale — retryable, and the retry works from the new status;
// otherwise cause is returned unchanged.
func staleIfMoved(c context.Context, repo reservation.Repository, r *reservation.Reservation, cause error) error {
	cur, err := repo.FindByOrderID(c, r.OrderID())
	if err != nil || cur.Status() == r.LoadedStatus() {
		return cause
	}

	// cause is text only: wrapping it too would let the consumer find its
	// kernel.Error and drop the message instead of retrying.
	return fmt.Errorf("reservation %s moved %s -> %s while it was handled (%s): %w", r.OrderID(), r.LoadedStatus(), cur.Status(), cause.Error(), reservation.ErrStale)
}
