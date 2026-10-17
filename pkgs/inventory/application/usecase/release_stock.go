package usecase

import (
	"context"
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/mapper"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/stock"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// ReleaseStock reacts to an order being cancelled: a RESERVED reservation
// gives its stock back. Rejected / already released reservations fail with a
// domain error, which the consumer treats as final.
type ReleaseStock struct {
	reservations reservation.Repository
	stocks       stock.Repository
	tx           port.Transactor
	events       port.EventPublisher
	clock        port.Clock
}

func NewReleaseStock(reservations reservation.Repository, stocks stock.Repository, tx port.Transactor, events port.EventPublisher, clock port.Clock) *ReleaseStock {
	return &ReleaseStock{reservations: reservations, stocks: stocks, tx: tx, events: events, clock: clock}
}

func (uc *ReleaseStock) Execute(c context.Context, in dto.ReleaseStockInput) (*dto.ReservationOutput, error) {
	ctx := contextx.FromContext(c)

	if in.OrderID == "" {
		return nil, reservation.ErrInvalidOrderID
	}

	var r *reservation.Reservation
	err := uc.tx.Do(ctx, func(c context.Context) error {
		var err error
		r, err = uc.reservations.FindByOrderID(c, in.OrderID)
		if err != nil {
			return err
		}
		// A missing stock row is a precondition failure the domain reports (ErrStockMissing).
		stocks, err := loadStocks(c, uc.stocks, r.Lines())
		if err != nil {
			return err
		}
		if err := r.Release(stocks, uc.clock.Now()); err != nil {
			return err
		}
		if err := saveStocks(c, uc.stocks, stocks); err != nil {
			return err
		}
		if err := uc.reservations.Update(c, r); err != nil {
			return fmt.Errorf("update reservation: %w", err)
		}

		return uc.events.Publish(c, r.PullEvents()...)
	})
	if err != nil {
		return nil, err
	}

	return mapper.ToReservationOutput(r), nil
}
