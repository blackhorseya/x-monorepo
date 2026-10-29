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

// CancelOrder cancels a pending order. It is driven by BOTH the rpc adapter
// (client request) and the consumer adapter (OrderCancellationRequested event).
type CancelOrder struct {
	repo   order.Repository
	events port.EventPublisher
	clock  port.Clock
	own    *ownership.Policy
}

func NewCancelOrder(repo order.Repository, events port.EventPublisher, clock port.Clock, own *ownership.Policy) *CancelOrder {
	return &CancelOrder{repo: repo, events: events, clock: clock, own: own}
}

func (uc *CancelOrder) Execute(c context.Context, in dto.CancelOrderInput) (*dto.OrderOutput, error) {
	ctx := contextx.FromContext(c)

	if in.OrderID == "" {
		return nil, order.ErrInvalidID
	}

	var o *order.Order
	err := retryOnConflict(ctx, func() error {
		var err error
		if o, err = uc.repo.FindByID(ctx, order.ID(in.OrderID)); err != nil {
			return err
		}
		if err = o.Cancel(uc.clock.Now()); err != nil {
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

	if in.Reason != "" {
		ctx.Info("order cancelled", "order_id", in.OrderID, "reason", in.Reason)
	}

	return mapper.ToOrderOutput(o), nil
}
