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

// CommitStock reacts to an order being completed: its RESERVED reservation
// becomes a sale, the reserved quantity leaving on-hand. Anything but RESERVED
// (released, committed, rejected, tombstoned) is a domain error the consumer
// treats as final. Unlike ReleaseStock there is no tombstone branch: an order
// is only completable once CONFIRMED, i.e. after inventory reserved it, so a
// missing reservation is an anomaly (ErrNotFound → drop), not an ordering race.
// Nor can a cancel and a complete both land: ordering's Save compares the
// version, so exactly one of OrderCancelled / OrderCompleted exists per order.
type CommitStock struct {
	reservations reservation.Repository
	stocks       stock.Repository
	events       port.EventPublisher
	clock        port.Clock
}

func NewCommitStock(reservations reservation.Repository, stocks stock.Repository, events port.EventPublisher, clock port.Clock) *CommitStock {
	return &CommitStock{reservations: reservations, stocks: stocks, events: events, clock: clock}
}

func (uc *CommitStock) Execute(c context.Context, in dto.CommitStockInput) (*dto.ReservationOutput, error) {
	ctx := contextx.FromContext(c)

	if in.OrderID == "" {
		return nil, reservation.ErrInvalidOrderID
	}

	r, err := uc.reservations.FindByOrderID(ctx, in.OrderID)
	if err != nil {
		return nil, err
	}
	// A missing stock row is a precondition failure the domain reports (ErrStockMissing).
	stocks, err := loadStocks(ctx, uc.stocks, r.Lines())
	if err != nil {
		return nil, err
	}
	if err = r.Commit(stocks, uc.clock.Now()); err != nil {
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
