// Package memory holds in-memory repositories for local dev and tests. They
// store copies so callers can't mutate stored state by aliasing, and they
// honour the same contracts as infra/postgres (ErrStale, ErrDuplicate).
package memory

import (
	"context"
	"slices"
	"sync"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/stock"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

var _ stock.Repository = (*StockRepository)(nil)

type StockRepository struct {
	mu   sync.RWMutex
	data map[stock.SKU]*stock.Stock
}

func NewStockRepository() *StockRepository {
	return &StockRepository{data: make(map[stock.SKU]*stock.Stock)}
}

func (i *StockRepository) FindBySKU(c context.Context, sku stock.SKU) (*stock.Stock, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	s, ok := i.data[sku]
	if !ok {
		return nil, stock.ErrNotFound
	}

	return cloneStock(s, s.Version())
}

// Save follows the Postgres contract: Version 0 inserts and loses (ErrStale)
// if the SKU exists; anything else updates only if the stored version still
// matches (ErrStale otherwise). The stored copy's version advances by one.
func (i *StockRepository) Save(c context.Context, s *stock.Stock) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	cur, exists := i.data[s.SKU()]
	switch {
	case s.Version() == 0 && exists:
		return stock.ErrStale
	case s.Version() != 0 && (!exists || cur.Version() != s.Version()):
		return stock.ErrStale
	}
	cp, err := cloneStock(s, s.Version()+1)
	if err != nil {
		return err
	}
	i.data[s.SKU()] = cp

	return nil
}

func (i *StockRepository) List(c context.Context, p stock.ListParams) ([]*stock.Stock, stock.SKU, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	skus := make([]stock.SKU, 0, len(i.data))
	for sku := range i.data {
		if sku > p.After {
			skus = append(skus, sku)
		}
	}
	slices.Sort(skus)
	var next stock.SKU
	if p.Limit > 0 && len(skus) > p.Limit {
		skus = skus[:p.Limit]
		next = skus[p.Limit-1]
	}
	out := make([]*stock.Stock, 0, len(skus))
	for _, sku := range skus {
		cp, err := cloneStock(i.data[sku], i.data[sku].Version())
		if err != nil {
			return nil, "", err
		}
		out = append(out, cp)
	}

	return out, next, nil
}

func cloneStock(s *stock.Stock, version int64) (*stock.Stock, error) {
	return stock.Reconstitute(stock.ReconstituteParams{
		SKU:       s.SKU(),
		OnHand:    s.OnHand(),
		Reserved:  s.Reserved(),
		Version:   version,
		UpdatedAt: s.UpdatedAt(),
	})
}
