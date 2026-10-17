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

// ReleaseStock reacts to an order being cancelled: a RESERVED reservation
// gives its stock back. Rejected / already released reservations fail with a
// domain error, which the consumer treats as final. No reservation at all
// means OrderCancelled overtook OrderCreated (topics are not ordered): a
// CANCELLED tombstone is written so the late OrderCreated reserves nothing.
type ReleaseStock struct {
	reservations reservation.Repository
	stocks       stock.Repository
	events       port.EventPublisher
	clock        port.Clock
}

func NewReleaseStock(reservations reservation.Repository, stocks stock.Repository, events port.EventPublisher, clock port.Clock) *ReleaseStock {
	return &ReleaseStock{reservations: reservations, stocks: stocks, events: events, clock: clock}
}

func (uc *ReleaseStock) Execute(c context.Context, in dto.ReleaseStockInput) (*dto.ReservationOutput, error) {
	ctx := contextx.FromContext(c)

	if in.OrderID == "" {
		return nil, reservation.ErrInvalidOrderID
	}

	r, err := uc.reservations.FindByOrderID(ctx, in.OrderID)
	switch {
	case errors.Is(err, reservation.ErrNotFound):
		return uc.tombstone(ctx, in.OrderID)
	case err != nil:
		return nil, err
	}
	// A missing stock row is a precondition failure the domain reports (ErrStockMissing).
	stocks, err := loadStocks(ctx, uc.stocks, r.Lines())
	if err != nil {
		return nil, err
	}
	if err = r.Release(stocks, uc.clock.Now()); err != nil {
		return nil, err
	}
	if err = saveStocks(ctx, uc.stocks, stocks); err != nil {
		return nil, err
	}
	if err = uc.reservations.Update(ctx, r); err != nil {
		return nil, fmt.Errorf("update reservation: %w", err)
	}
	if err = uc.events.Publish(ctx, r.PullEvents()...); err != nil {
		return nil, err
	}

	return mapper.ToReservationOutput(r), nil
}

// tombstone records the cancellation of an order inventory has not seen. If a
// concurrent ReserveStock wins the insert, the duplicate is reported as a
// concurrent update (not a kernel error) so the consumer retries and releases
// what that reservation just took.
func (uc *ReleaseStock) tombstone(c context.Context, orderID string) (*dto.ReservationOutput, error) {
	r, err := reservation.Tombstone(orderID, uc.clock.Now())
	if err != nil {
		return nil, err
	}
	if err := uc.reservations.Add(c, r); err != nil {
		if errors.Is(err, reservation.ErrDuplicate) {
			return nil, fmt.Errorf("add tombstone %s: %w", orderID, kernel.ErrConcurrentUpdate)
		}

		return nil, fmt.Errorf("add tombstone %s: %w", orderID, err)
	}

	return mapper.ToReservationOutput(r), nil
}
