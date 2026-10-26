package usecase

import (
	"context"
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/mapper"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// CompleteOrder fulfils a CONFIRMED order (→ COMPLETED) and raises OrderCompleted,
// on which inventory commits the reservation: the stock is sold, not held.
// Driven by the rpc adapter only. Any other status refuses with
// order.ErrCannotComplete — including CANCELLED, so a complete racing a cancel
// cannot both win: Save compares the version, the loser reloads and is refused.
type CompleteOrder struct {
	repo   order.Repository
	events port.EventPublisher
	clock  port.Clock
}

func NewCompleteOrder(repo order.Repository, events port.EventPublisher, clock port.Clock) *CompleteOrder {
	return &CompleteOrder{repo: repo, events: events, clock: clock}
}

func (uc *CompleteOrder) Execute(c context.Context, in dto.CompleteOrderInput) (*dto.OrderOutput, error) {
	ctx := contextx.FromContext(c)

	if in.OrderID == "" {
		return nil, order.ErrInvalidID
	}

	o, err := uc.repo.FindByID(ctx, order.ID(in.OrderID))
	if err != nil {
		return nil, err
	}

	if err = o.Complete(uc.clock.Now()); err != nil {
		return nil, err
	}

	if err = uc.repo.Save(ctx, o); err != nil {
		return nil, fmt.Errorf("save order: %w", err)
	}
	if err = uc.events.Publish(ctx, o.PullEvents()...); err != nil {
		return nil, err
	}

	return mapper.ToOrderOutput(o), nil
}
