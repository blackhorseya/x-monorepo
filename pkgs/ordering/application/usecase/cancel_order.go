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

// CancelOrder cancels a pending order. It is driven by BOTH the rpc adapter
// (client request) and the consumer adapter (OrderCancellationRequested event).
type CancelOrder struct {
	repo   order.Repository
	tx     port.Transactor
	events port.EventPublisher
	clock  port.Clock
}

func NewCancelOrder(repo order.Repository, tx port.Transactor, events port.EventPublisher, clock port.Clock) *CancelOrder {
	return &CancelOrder{repo: repo, tx: tx, events: events, clock: clock}
}

func (uc *CancelOrder) Execute(c context.Context, in dto.CancelOrderInput) (*dto.OrderOutput, error) {
	ctx := contextx.FromContext(c)

	if in.OrderID == "" {
		return nil, order.ErrInvalidID
	}

	o, err := uc.repo.FindByID(ctx, order.ID(in.OrderID))
	if err != nil {
		return nil, err
	}

	if err = o.Cancel(uc.clock.Now()); err != nil {
		return nil, err
	}

	err = uc.tx.Do(ctx, func(c context.Context) error {
		if err := uc.repo.Save(c, o); err != nil {
			return fmt.Errorf("save order: %w", err)
		}

		return uc.events.Publish(c, o.PullEvents()...)
	})
	if err != nil {
		return nil, err
	}

	if in.Reason != "" {
		ctx.Info("order cancelled", "order_id", in.OrderID, "reason", in.Reason)
	}

	return mapper.ToOrderOutput(o), nil
}
