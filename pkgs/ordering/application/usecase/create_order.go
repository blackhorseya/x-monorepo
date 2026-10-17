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

// CreateOrder places a new pending order and publishes OrderCreated in the
// same transaction (transactional outbox: see port.Transactor).
type CreateOrder struct {
	repo   order.Repository
	tx     port.Transactor
	events port.EventPublisher
	ids    port.IDGenerator
	clock  port.Clock
}

func NewCreateOrder(repo order.Repository, tx port.Transactor, events port.EventPublisher, ids port.IDGenerator, clock port.Clock) *CreateOrder {
	return &CreateOrder{repo: repo, tx: tx, events: events, ids: ids, clock: clock}
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

	err = uc.tx.Do(ctx, func(c context.Context) error {
		if err := uc.repo.Save(c, o); err != nil {
			return fmt.Errorf("save order: %w", err)
		}

		return uc.events.Publish(c, o.PullEvents()...)
	})
	if err != nil {
		return nil, err
	}

	return mapper.ToOrderOutput(o), nil
}
