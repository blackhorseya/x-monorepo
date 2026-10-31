package memory

import (
	"context"
	"sync"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/port"
)

var _ port.ProductCatalog = (*ProductCatalog)(nil)

// ProductCatalog is a port.ProductCatalog over a fixed list of SKUs, for tests and for composing
// inventory without a catalog. It counts the questions it was asked and can be made to fail.
type ProductCatalog struct {
	mu    sync.Mutex
	skus  map[string]struct{}
	all   bool
	calls int
	err   error
}

// NewProductCatalog is a catalog with exactly these products.
func NewProductCatalog(skus ...string) *ProductCatalog {
	l := &ProductCatalog{skus: map[string]struct{}{}}
	for _, s := range skus {
		l.skus[s] = struct{}{}
	}

	return l
}

// NewAnyProductCatalog has every SKU as a product: for tests that stock but do not care
// about the catalog.
func NewAnyProductCatalog() *ProductCatalog {
	l := NewProductCatalog()
	l.all = true

	return l
}

// Add puts sku in the catalog.
func (i *ProductCatalog) Add(sku string) *ProductCatalog {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.skus[sku] = struct{}{}

	return i
}

// FailWith makes every following HasProduct return err.
func (i *ProductCatalog) FailWith(err error) *ProductCatalog {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.err = err

	return i
}

// Calls is how many times the catalog was asked.
func (i *ProductCatalog) Calls() int {
	i.mu.Lock()
	defer i.mu.Unlock()

	return i.calls
}

func (i *ProductCatalog) HasProduct(_ context.Context, sku string) (bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls++
	if i.err != nil {
		return false, i.err
	}
	_, ok := i.skus[sku]

	return ok || i.all, nil
}
