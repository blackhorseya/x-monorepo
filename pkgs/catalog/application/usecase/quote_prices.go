package usecase

import (
	"context"
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/catalog/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/application/mapper"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/domain/product"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// QuotePrices is the batch price lookup other bounded contexts will call
// through a port of their own: one query for all SKUs, and an answer that
// tells a SKU with no product from one that is no longer sold.
type QuotePrices struct {
	products product.Repository
}

func NewQuotePrices(products product.Repository) *QuotePrices {
	return &QuotePrices{products: products}
}

func (uc *QuotePrices) Execute(c context.Context, in dto.QuotePricesInput) (*dto.QuotePricesOutput, error) {
	out := &dto.QuotePricesOutput{Prices: make(map[string]dto.PriceOutput, len(in.SKUs))}
	if len(in.SKUs) == 0 {
		return out, nil
	}

	skus := make([]product.SKU, 0, len(in.SKUs))
	for _, sku := range in.SKUs {
		if sku == "" {
			return nil, product.ErrInvalidSKU
		}
		skus = append(skus, product.SKU(sku))
	}
	found, err := uc.products.FindBySKUs(contextx.FromContext(c), skus)
	if err != nil {
		return nil, fmt.Errorf("find %d products: %w", len(skus), err)
	}

	seen := make(map[string]struct{}, len(in.SKUs))
	for _, sku := range in.SKUs {
		if _, dup := seen[sku]; dup {
			continue
		}
		seen[sku] = struct{}{}

		switch p := found[product.SKU(sku)]; {
		case p == nil:
			out.Missing = append(out.Missing, sku)
		case !p.Active():
			out.Inactive = append(out.Inactive, sku)
		default:
			out.Prices[sku] = mapper.ToPriceOutput(p.Price())
		}
	}

	return out, nil
}
