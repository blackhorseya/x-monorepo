// Package pricing adapts the catalog's contract to ordering's port.PriceQuoter: ordering
// asks what a SKU costs; the answer is the catalog's, nothing of its model comes across.
package pricing

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	catalogv1 "github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1/catalogv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/port"
)

var _ port.PriceQuoter = (*Quoter)(nil)

// Quoter asks the catalog's service handler in process (no network hop, no interceptors):
// apps/rpc binds the catalog's own CatalogService to catalogv1connect.CatalogServiceHandler.
// Only the generated contract is imported, never the catalog's domain or application.
type Quoter struct {
	catalog catalogv1connect.CatalogServiceHandler
}

func NewQuoter(catalog catalogv1connect.CatalogServiceHandler) *Quoter {
	return &Quoter{catalog: catalog}
}

func (i *Quoter) QuotePrices(c context.Context, skus []string) (*dto.PriceQuote, error) {
	out := &dto.PriceQuote{Prices: make(map[string]dto.Money, len(skus))}
	if len(skus) == 0 {
		return out, nil
	}

	res, err := i.catalog.QuotePrices(c, connect.NewRequest(&catalogv1.QuotePricesRequest{Skus: skus}))
	if err != nil {
		return nil, fmt.Errorf("catalog quote: %w", err)
	}
	for sku, m := range res.Msg.GetPrices() {
		out.Prices[sku] = dto.Money{AmountMinor: m.GetAmountMinor(), Currency: m.GetCurrency()}
	}
	out.Missing = res.Msg.GetMissing()
	out.Inactive = res.Msg.GetInactive()

	return out, nil
}
