// Package catalog adapts the catalog's contract to inventory's port.ProductCatalog: inventory
// asks whether a SKU is a product; the answer is the catalog's, nothing of its model comes across.
package catalog

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	catalogv1 "github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1/catalogv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/port"
)

var _ port.ProductCatalog = (*Catalog)(nil)

// Catalog asks the catalog's service handler in process (no network hop, no interceptors):
// apps/rpc binds the catalog's own CatalogService to catalogv1connect.CatalogServiceHandler.
// Only the generated contract is imported, never the catalog's domain or application.
type Catalog struct {
	catalog catalogv1connect.CatalogServiceHandler
}

func NewCatalog(catalog catalogv1connect.CatalogServiceHandler) *Catalog {
	return &Catalog{catalog: catalog}
}

// HasProduct is GetProduct: a product exists whether it is on sale or not (the catalog never
// deletes one). NOT_FOUND is "no such product"; any other failure is the catalog not
// answering, so a SKU is never let through on a guess.
func (i *Catalog) HasProduct(c context.Context, sku string) (bool, error) {
	_, err := i.catalog.GetProduct(c, connect.NewRequest(&catalogv1.GetProductRequest{Sku: sku}))
	switch {
	case err == nil:
		return true, nil
	case connect.CodeOf(err) == connect.CodeNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("catalog get product %s: %w: %w", sku, port.ErrCatalogUnavailable, err)
	}
}
