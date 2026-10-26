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

// ConfirmOrder moves a pending order to CONFIRMED once inventory reserved every
// line. Driven by the consumer adapter (inventory.v1.StockReserved); there is no
// RPC for it on purpose — confirmation is inventory's answer, not a client call.
// An order already cancelled refuses (order.ErrCannotConfirm → the consumer drops
// the late event; inventory releases that reservation on OrderCancelled).
type ConfirmOrder struct {
	repo   order.Repository
	events port.EventPublisher
	clock  port.Clock
}

func NewConfirmOrder(repo order.Repository, events port.EventPublisher, clock port.Clock) *ConfirmOrder {
	return &ConfirmOrder{repo: repo, events: events, clock: clock}
}

func (uc *ConfirmOrder) Execute(c context.Context, in dto.ConfirmOrderInput) (*dto.OrderOutput, error) {
	ctx := contextx.FromContext(c)

	if in.OrderID == "" {
		return nil, order.ErrInvalidID
	}

	o, err := uc.repo.FindByID(ctx, order.ID(in.OrderID))
	if err != nil {
		return nil, err
	}

	if err = o.Confirm(uc.clock.Now()); err != nil {
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
