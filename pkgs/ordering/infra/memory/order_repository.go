// Package memory is an in-memory order.Repository for local dev and tests.
// A real implementation goes under ordering/infra/postgres with its own row
// models; driver errors are translated to domain errors there.
package memory

import (
	"context"
	"sort"
	"sync"
	"time"

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

// FindByIDForCustomer is FindByID for one customer's orders: another
// customer's order is ErrNotFound, the same as a missing one.
func (i *OrderRepository) FindByIDForCustomer(c context.Context, id order.ID, customerID string) (*order.Order, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	o, ok := i.data[id]
	if !ok || o.CustomerID() != customerID {
		return nil, order.ErrNotFound
	}

	return clone(o)
}

// Save follows the Postgres contract: Version 0 inserts (ErrDuplicate if the id
// is taken); anything else updates only if the stored version still matches
// (ErrStale otherwise). The stored copy's version advances by one.
func (i *OrderRepository) Save(c context.Context, o *order.Order) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	cur, exists := i.data[o.ID()]
	switch {
	case o.Version() == 0 && exists:
		return order.ErrDuplicate
	case o.Version() == 0 && i.keyTaken(o):
		return order.ErrIdempotencyConflict
	case o.Version() != 0 && (!exists || cur.Version() != o.Version()):
		return order.ErrStale
	}

	cp, err := cloneAt(o, o.Version()+1)
	if err != nil {
		return err
	}
	i.data[o.ID()] = cp

	return nil
}

// keyTaken reports whether another order of the same customer already owns o's idempotency key.
func (i *OrderRepository) keyTaken(o *order.Order) bool {
	if o.IdempotencyKey() == "" {
		return false
	}
	_, taken := i.byKey(o.CustomerID(), o.IdempotencyKey())

	return taken
}

func (i *OrderRepository) byKey(customerID, key string) (*order.Order, bool) {
	for _, o := range i.data {
		if o.CustomerID() == customerID && o.IdempotencyKey() == key {
			return o, true
		}
	}

	return nil, false
}

// FindByIdempotencyKey is the (customer, key) lookup of the Postgres unique index.
func (i *OrderRepository) FindByIdempotencyKey(c context.Context, customerID, key string) (*order.Order, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	o, ok := i.byKey(customerID, key)
	if !ok || key == "" {
		return nil, order.ErrNotFound
	}

	return clone(o)
}

func clone(o *order.Order) (*order.Order, error) { return cloneAt(o, o.Version()) }

func cloneAt(o *order.Order, version int64) (*order.Order, error) {
	return order.Reconstitute(order.ReconstituteParams{
		ID:         o.ID(),
		CustomerID: o.CustomerID(),
		Items:      o.Items(),
		Status:     o.Status(),
		CreatedAt:  o.CreatedAt(),
		UpdatedAt:  o.UpdatedAt(),
		Version:    version,

		IdempotencyKey: o.IdempotencyKey(),
		RequestHash:    o.RequestHash(),
	})
}

// List filters, sorts newest first (created_at desc, id desc) and applies the
// keyset cursor exactly like the Postgres implementation does in SQL.
func (i *OrderRepository) List(c context.Context, p order.ListParams) ([]*order.Order, *order.Cursor, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	matches := make([]*order.Order, 0, len(i.data))
	for _, o := range i.data {
		if p.CustomerID != "" && o.CustomerID() != p.CustomerID {
			continue
		}
		if p.Status != nil && o.Status() != *p.Status {
			continue
		}
		if p.After != nil && !p.After.Before(o.CreatedAt(), o.ID()) {
			continue
		}
		matches = append(matches, o)
	}
	sort.Slice(matches, func(a, b int) bool {
		x, y := matches[a], matches[b]
		if !x.CreatedAt().Equal(y.CreatedAt()) {
			return x.CreatedAt().After(y.CreatedAt())
		}
		return x.ID() > y.ID()
	})

	var next *order.Cursor
	if p.Limit > 0 && len(matches) > p.Limit {
		matches = matches[:p.Limit]
		cur := order.CursorOf(matches[p.Limit-1])
		next = &cur
	}

	out := make([]*order.Order, 0, len(matches))
	for _, o := range matches {
		cp, err := clone(o)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, cp)
	}

	return out, next, nil
}

// CountByStatus mirrors the Postgres GROUP BY: statuses with no order are absent.
func (i *OrderRepository) CountByStatus(c context.Context) ([]order.StatusCount, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	type acc struct {
		n      int64
		oldest time.Time
	}
	by := map[order.Status]*acc{}
	for _, o := range i.data {
		a := by[o.Status()]
		if a == nil {
			a = &acc{oldest: o.UpdatedAt()}
			by[o.Status()] = a
		}
		a.n++
		if o.UpdatedAt().Before(a.oldest) {
			a.oldest = o.UpdatedAt()
		}
	}

	out := make([]order.StatusCount, 0, len(by))
	for _, st := range order.Statuses() {
		if a, ok := by[st]; ok {
			out = append(out, order.NewStatusCount(st, a.n, a.oldest))
		}
	}

	return out, nil
}
