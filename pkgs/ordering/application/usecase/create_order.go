// Package usecase orchestrates domain objects (go-kit's "service"). Business
// rules stay in the domain; cross-cutting concerns are usecasex decorators.
package usecase

import (
	"context"
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/mapper"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// CreateOrder places a new pending order.
type CreateOrder struct {
	repo  order.Repository
	ids   port.IDGenerator
	clock port.Clock
}

func NewCreateOrder(repo order.Repository, ids port.IDGenerator, clock port.Clock) *CreateOrder {
	return &CreateOrder{repo: repo, ids: ids, clock: clock}
}

func (uc *CreateOrder) Execute(c context.Context, in dto.CreateOrderInput) (*dto.OrderOutput, error) {
	ctx := contextx.FromContext(c)

	items := make([]order.LineItem, 0, len(in.Items))
	for _, it := range in.Items {
		price, err := kernel.NewMoney(it.UnitPrice.AmountMinor, it.UnitPrice.Currency)
		if err != nil {
			return nil, err
		}
		li, err := order.NewLineItem(it.SKU, it.Quantity, price)
		if err != nil {
			return nil, err
		}
		items = append(items, li)
	}

	id, err := uc.ids.NewID(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate order id: %w", err)
	}

	o, err := order.New(order.Params{ID: order.ID(id), CustomerID: in.CustomerID, Items: items, Now: uc.clock.Now()})
	if err != nil {
		return nil, err
	}

	if err = uc.repo.Save(ctx, o); err != nil {
		return nil, fmt.Errorf("save order: %w", err)
	}

	return mapper.ToOrderOutput(o), nil
}
