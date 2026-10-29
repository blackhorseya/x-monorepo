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
// The price of every line is the catalog's (port.PriceQuoter), read when the order is
// placed and stored on the line as a snapshot; the caller supplies only sku and quantity.
// An unknown or inactive SKU, or lines in more than one currency, refuse the order.
//
// With an IdempotencyKey the call is safe to retry. The key is scoped to the
// resolved customer: the same key and lines return the original order (as it is
// now) without writing, publishing or asking the catalog; the same key with other lines is
// ErrIdempotencyKeyReused. Two concurrent first uses are settled by the store's
// unique constraint (Save → ErrIdempotencyConflict), never by a lock here.
// The key itself is never logged or put in an error.
type CreateOrder struct {
	repo   order.Repository
	events port.EventPublisher
	ids    port.IDGenerator
	clock  port.Clock
	own    *ownership.Policy
	prices port.PriceQuoter
}

func NewCreateOrder(repo order.Repository, events port.EventPublisher, ids port.IDGenerator, clock port.Clock, own *ownership.Policy, prices port.PriceQuoter) *CreateOrder {
	return &CreateOrder{repo: repo, events: events, ids: ids, clock: clock, own: own, prices: prices}
}

func (uc *CreateOrder) Execute(c context.Context, in dto.CreateOrderInput) (*dto.OrderOutput, error) {
	ctx := contextx.FromContext(c)

	customerID, err := uc.own.ForCreate(ctx, in.CustomerID)
	if err != nil {
		return nil, err
	}

	lines := make([]order.RequestLine, 0, len(in.Items))
	for _, it := range in.Items {
		l, err := order.NewRequestLine(it.SKU, it.Quantity)
		if err != nil {
			return nil, err
		}
		lines = append(lines, l)
	}

	// A retry of a request that already created an order is answered from that order,
	// before the catalog is asked: the product may have been repriced or taken off sale
	// since, and the order's own snapshot is what the caller paid.
	if in.IdempotencyKey != "" {
		prior, err := uc.repo.FindByIdempotencyKey(ctx, customerID, in.IdempotencyKey)
		switch {
		case err == nil:
			return replay(prior, lines)
		case !errors.Is(err, order.ErrNotFound):
			return nil, fmt.Errorf("find order by idempotency key: %w", err)
		}
	}

	items, err := uc.price(ctx, lines)
	if err != nil {
		return nil, err
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

		return replay(prior, lines)
	}
	if err := uc.events.Publish(ctx, o.PullEvents()...); err != nil {
		return nil, err
	}

	return mapper.ToOrderOutput(o), nil
}

// price asks the catalog once for every distinct SKU of the order and turns the lines into
// priced items: the price written on each is the snapshot the order keeps. A SKU the catalog
// cannot quote refuses the whole order here, before anything is written.
func (uc *CreateOrder) price(ctx context.Context, lines []order.RequestLine) ([]order.LineItem, error) {
	skus := make([]string, 0, len(lines))
	seen := make(map[string]struct{}, len(lines))
	for _, l := range lines {
		if _, dup := seen[l.SKU()]; !dup {
			seen[l.SKU()] = struct{}{}
			skus = append(skus, l.SKU())
		}
	}

	quote, err := uc.prices.QuotePrices(ctx, skus)
	if err != nil {
		return nil, fmt.Errorf("quote prices of %d skus: %w", len(skus), err)
	}
	if len(quote.Missing) > 0 {
		return nil, order.ErrUnknownSKU
	}
	if len(quote.Inactive) > 0 {
		return nil, order.ErrInactiveSKU
	}

	items := make([]order.LineItem, 0, len(lines))
	for _, l := range lines {
		p, ok := quote.Prices[l.SKU()]
		if !ok {
			// The catalog answered neither with a price nor with a reason: it cannot
			// price this SKU, so the order cannot be placed.
			return nil, order.ErrUnknownSKU
		}
		price, err := kernel.NewMoney(p.AmountMinor, p.Currency)
		if err != nil {
			return nil, fmt.Errorf("catalog price of %s: %w", l.SKU(), err)
		}
		li, err := order.NewLineItem(l.SKU(), l.Quantity(), price)
		if err != nil {
			return nil, err
		}
		items = append(items, li)
	}

	return items, nil
}

// replay answers a request whose key already created prior: the same lines get
// the original order back (nothing written, nothing published); other lines are
// a reused key.
func replay(prior *order.Order, lines []order.RequestLine) (*dto.OrderOutput, error) {
	if !prior.IsReplayOf(lines) {
		return nil, order.ErrIdempotencyKeyReused
	}

	out := mapper.ToOrderOutput(prior)
	out.Replayed = true

	return out, nil
}
