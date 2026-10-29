package port

import (
	"context"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
)

// PriceQuoter is what CreateOrder needs from the catalog: the current price of each SKU,
// asked once for the whole order. Defined here, on the consumer's side; adapters/pricing
// implements it over the catalog's contract.
type PriceQuoter interface {
	// QuotePrices answers every SKU asked as a price (active products), or as missing (no
	// such product) or inactive (taken off sale). An error is the catalog failing to
	// answer, not a SKU it cannot quote.
	QuotePrices(c context.Context, skus []string) (*dto.PriceQuote, error)
}
