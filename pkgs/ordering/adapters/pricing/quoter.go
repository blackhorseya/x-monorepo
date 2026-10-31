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

// Quoter asks the catalog through catalogv1connect.CatalogServiceHandler: apps/rpc binds it to the
// Connect client of catalog-rpc (its UNAVAILABLE is "the catalog did not answer").
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
		if connect.CodeOf(err) == connect.CodeUnavailable {
			// The catalog did not answer (the client adapter says so with UNAVAILABLE): the order is not
			// wrong, it cannot be priced right now. Wrapped, the cause stays for the log.
			return nil, fmt.Errorf("catalog quote: %w: %w", port.ErrCatalogUnavailable, err)
		}

		return nil, fmt.Errorf("catalog quote: %w", err)
	}
	for sku, m := range res.Msg.GetPrices() {
		out.Prices[sku] = dto.Money{AmountMinor: m.GetAmountMinor(), Currency: m.GetCurrency()}
	}
	out.Missing = res.Msg.GetMissing()
	out.Inactive = res.Msg.GetInactive()

	return out, nil
}
