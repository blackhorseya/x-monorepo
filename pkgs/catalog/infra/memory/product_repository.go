// Package memory holds in-memory repositories for local dev and tests. They
// store copies so callers can't mutate stored state by aliasing, and they
// honour the same contracts as infra/postgres (ErrStale).
package memory

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/blackhorseya/x-monorepo/pkgs/catalog/domain/product"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

var _ product.Repository = (*ProductRepository)(nil)

type ProductRepository struct {
	mu   sync.RWMutex
	data map[product.SKU]*product.Product
}

func NewProductRepository() *ProductRepository {
	return &ProductRepository{data: make(map[product.SKU]*product.Product)}
}

func (i *ProductRepository) FindBySKU(c context.Context, sku product.SKU) (*product.Product, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	p, ok := i.data[sku]
	if !ok {
		return nil, product.ErrNotFound
	}

	return cloneProduct(p, p.Version())
}

func (i *ProductRepository) FindBySKUs(c context.Context, skus []product.SKU) (map[product.SKU]*product.Product, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	out := make(map[product.SKU]*product.Product, len(skus))
	for _, sku := range skus {
		p, ok := i.data[sku]
		if !ok {
			continue
		}
		cp, err := cloneProduct(p, p.Version())
		if err != nil {
			return nil, err
		}
		out[sku] = cp
	}

	return out, nil
}

// Save follows the Postgres contract: Version 0 inserts and loses (ErrStale)
// if the SKU exists; anything else updates only if the stored version still
// matches (ErrStale otherwise). The stored copy's version advances by one.
func (i *ProductRepository) Save(c context.Context, p *product.Product) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	cur, exists := i.data[p.SKU()]
	switch {
	case p.Version() == 0 && exists:
		return product.ErrStale
	case p.Version() != 0 && (!exists || cur.Version() != p.Version()):
		return product.ErrStale
	}
	cp, err := cloneProduct(p, p.Version()+1)
	if err != nil {
		return err
	}
	i.data[p.SKU()] = cp

	return nil
}

func (i *ProductRepository) List(c context.Context, p product.ListParams) ([]*product.Product, product.SKU, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	skus := make([]product.SKU, 0, len(i.data))
	for sku := range i.data {
		if sku > p.After && strings.HasPrefix(sku.String(), p.SKUPrefix) {
			skus = append(skus, sku)
		}
	}
	slices.Sort(skus)
	var next product.SKU
	if p.Limit > 0 && len(skus) > p.Limit {
		skus = skus[:p.Limit]
		next = skus[p.Limit-1]
	}
	out := make([]*product.Product, 0, len(skus))
	for _, sku := range skus {
		cp, err := cloneProduct(i.data[sku], i.data[sku].Version())
		if err != nil {
			return nil, "", err
		}
		out = append(out, cp)
	}

	return out, next, nil
}

func cloneProduct(p *product.Product, version int64) (*product.Product, error) {
	return product.Reconstitute(product.ReconstituteParams{
		SKU:       p.SKU(),
		Name:      p.Name(),
		Price:     p.Price(),
		Active:    p.Active(),
		Version:   version,
		UpdatedAt: p.UpdatedAt(),
	})
}
