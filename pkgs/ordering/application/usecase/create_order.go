// Package usecase orchestrates domain objects (go-kit's "service"). Business
// rules stay in the domain; cross-cutting concerns are usecasex decorators.
package usecase

import (
	"context"
	"errors"
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/mapper"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/ownership"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// CreateOrder places a new pending order and publishes OrderCreated. The
// provider runs it inside one transaction (usecasex.Transactional), so the
// order row and the outbox message commit together.
//
// With an IdempotencyKey the call is safe to retry. The key is scoped to the
// resolved customer: the same key and items return the original order (as it is
// now) without writing or publishing; the same key with other items is
// ErrIdempotencyKeyReused. Two concurrent first uses are settled by the store's
// unique constraint (Save → ErrIdempotencyConflict), never by a lock here.
// The key itself is never logged or put in an error.
type CreateOrder struct {
	repo   order.Repository
	events port.EventPublisher
	ids    port.IDGenerator
	clock  port.Clock
	own    *ownership.Policy
}

func NewCreateOrder(repo order.Repository, events port.EventPublisher, ids port.IDGenerator, clock port.Clock, own *ownership.Policy) *CreateOrder {
	return &CreateOrder{repo: repo, events: events, ids: ids, clock: clock, own: own}
}

func (uc *CreateOrder) Execute(c context.Context, in dto.CreateOrderInput) (*dto.OrderOutput, error) {
	ctx := contextx.FromContext(c)

	customerID, err := uc.own.ForCreate(ctx, in.CustomerID)
	if err != nil {
		return nil, err
	}

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

	if in.IdempotencyKey != "" {
		prior, err := uc.repo.FindByIdempotencyKey(ctx, customerID, in.IdempotencyKey)
		switch {
		case err == nil:
			return replay(prior, items)
		case !errors.Is(err, order.ErrNotFound):
			return nil, fmt.Errorf("find order by idempotency key: %w", err)
		}
	}

	id, err := uc.ids.NewID(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate order id: %w", err)
	}

	o, err := order.New(order.Params{ID: order.ID(id), CustomerID: customerID, Items: items, Now: uc.clock.Now(), IdempotencyKey: in.IdempotencyKey})
	if err != nil {
		return nil, err
	}

	if err := uc.repo.Save(ctx, o); err != nil {
		if !errors.Is(err, order.ErrIdempotencyConflict) {
			return nil, fmt.Errorf("save order: %w", err)
		}
		// Lost the race for this key: the winner is committed (the store waited
		// for it), so read it back instead of creating a second order.
		prior, err := uc.repo.FindByIdempotencyKey(ctx, customerID, in.IdempotencyKey)
		if err != nil {
			return nil, fmt.Errorf("read back idempotent order: %w", err)
		}

		return replay(prior, items)
	}
	if err := uc.events.Publish(ctx, o.PullEvents()...); err != nil {
		return nil, err
	}

	return mapper.ToOrderOutput(o), nil
}

// replay answers a request whose key already created prior: the same items get
// the original order back (nothing written, nothing published); other items are
// a reused key.
func replay(prior *order.Order, items []order.LineItem) (*dto.OrderOutput, error) {
	if !prior.IsReplayOf(items) {
		return nil, order.ErrIdempotencyKeyReused
	}

	return mapper.ToOrderOutput(prior), nil
}
