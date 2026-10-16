// Package memory is an in-memory order.Repository for local dev and tests.
// A real implementation goes under ordering/infra/postgres with its own row
// models; driver errors are translated to domain errors there.
package memory

import (
	"context"
	"sync"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

var _ order.Repository = (*OrderRepository)(nil)

// OrderRepository stores copies so callers can't mutate stored state by aliasing.
type OrderRepository struct {
	mu   sync.RWMutex
	data map[order.ID]*order.Order
}

func NewOrderRepository() *OrderRepository {
	return &OrderRepository{data: make(map[order.ID]*order.Order)}
}

func (i *OrderRepository) FindByID(c context.Context, id order.ID) (*order.Order, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	o, ok := i.data[id]
	if !ok {
		return nil, order.ErrNotFound
	}

	return clone(o)
}

func (i *OrderRepository) Save(c context.Context, o *order.Order) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	cp, err := clone(o)
	if err != nil {
		return err
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	i.data[o.ID()] = cp

	return nil
}

func clone(o *order.Order) (*order.Order, error) {
	return order.Reconstitute(order.ReconstituteParams{
		ID:         o.ID(),
		CustomerID: o.CustomerID(),
		Items:      o.Items(),
		Status:     o.Status(),
		CreatedAt:  o.CreatedAt(),
		UpdatedAt:  o.UpdatedAt(),
	})
}
