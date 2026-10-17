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
	tx           port.Transactor
	events       port.EventPublisher
	clock        port.Clock
}

func NewReserveStock(reservations reservation.Repository, stocks stock.Repository, tx port.Transactor, events port.EventPublisher, clock port.Clock) *ReserveStock {
	return &ReserveStock{reservations: reservations, stocks: stocks, tx: tx, events: events, clock: clock}
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

	var r *reservation.Reservation
	err := uc.tx.Do(ctx, func(c context.Context) error {
		existing, err := uc.reservations.FindByOrderID(c, in.OrderID)
		switch {
		case err == nil:
			r = existing // already handled (redelivery)
			return nil
		case !errors.Is(err, reservation.ErrNotFound):
			return fmt.Errorf("find reservation: %w", err)
		}

		// Unknown SKUs are left out: the domain counts them as zero available.
		stocks, err := loadStocks(c, uc.stocks, lines)
		if err != nil {
			return err
		}

		r, err = reservation.Reserve(reservation.Params{OrderID: in.OrderID, Lines: lines, Now: uc.clock.Now()}, stocks)
		if err != nil {
			return err
		}
		if r.Status() == reservation.StatusReserved {
			if err := saveStocks(c, uc.stocks, stocks); err != nil {
				return err
			}
		}
		if err := uc.reservations.Add(c, r); err != nil {
			return fmt.Errorf("add reservation: %w", err)
		}

		return uc.events.Publish(c, r.PullEvents()...)
	})
	if err != nil {
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
