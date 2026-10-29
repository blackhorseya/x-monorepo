package usecase

import (
	"context"
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/mapper"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/ownership"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// CompleteOrder fulfils a CONFIRMED order (→ COMPLETED) and raises OrderCompleted,
// on which inventory commits the reservation: the stock is sold, not held.
// Driven by the rpc adapter only. Any other status refuses with
// order.ErrCannotComplete — including CANCELLED, so a complete racing a cancel
// cannot both win: Save compares the version, the loser reloads (retryOnConflict) and is refused.
type CompleteOrder struct {
	repo   order.Repository
	events port.EventPublisher
	clock  port.Clock
	own    *ownership.Policy
}

func NewCompleteOrder(repo order.Repository, events port.EventPublisher, clock port.Clock, own *ownership.Policy) *CompleteOrder {
	return &CompleteOrder{repo: repo, events: events, clock: clock, own: own}
}

func (uc *CompleteOrder) Execute(c context.Context, in dto.CompleteOrderInput) (*dto.OrderOutput, error) {
	ctx := contextx.FromContext(c)

	if in.OrderID == "" {
		return nil, order.ErrInvalidID
	}

	var o *order.Order
	err := retryOnConflict(ctx, func() error {
		var err error
		// Reloaded on every retry, so the owner condition rides along each time. A
		// confined caller's foreign order is order.not_found, as in GetOrder.
		if o, err = uc.find(ctx, in.OrderID); err != nil {
			return err
		}
		if err = o.Complete(uc.clock.Now()); err != nil {
			return err
		}
		if err = uc.repo.Save(ctx, o); err != nil {
			return fmt.Errorf("save order: %w", err)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}
	if err = uc.events.Publish(ctx, o.PullEvents()...); err != nil {
		return nil, err
	}

	return mapper.ToOrderOutput(o), nil
}

func (uc *CompleteOrder) find(ctx contextx.Contextx, id string) (*order.Order, error) {
	if customer, confined := uc.own.OwnOnly(ctx); confined {
		return uc.repo.FindByIDForCustomer(ctx, order.ID(id), customer)
	}

	return uc.repo.FindByID(ctx, order.ID(id))
}
