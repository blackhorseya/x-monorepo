// Package dto is the hand-written application boundary of the catalog use
// cases: only what they need, not a copy of the wire format.
package dto

import "time"

// PriceOutput is an amount in minor units (cents) and its ISO 4217 currency.
type PriceOutput struct {
	Amount   int64
	Currency string
}

// ProductOutput is one product as use cases return it.
type ProductOutput struct {
	SKU        string
	Name       string
	Price      PriceOutput
	Active     bool
	UpdateTime time.Time
}

// UpsertProductInput creates the product, or replaces the name and price of
// the existing one (and makes an inactive one active again).
type UpsertProductInput struct {
	SKU      string
	Name     string
	Amount   int64
	Currency string
}

type GetProductInput struct {
	SKU string
}

// ListProductsInput pages through products ascending by SKU. PageSize 0 is
// the server default; the page size policy lives in the use case.
type ListProductsInput struct {
	PageSize  int32
	PageToken string
	SKUPrefix string
}

type ListProductsOutput struct {
	Products      []ProductOutput
	NextPageToken string
}

type DeactivateProductInput struct {
	SKU string
}

// QuotePricesInput asks for the current price of each SKU.
type QuotePricesInput struct {
	SKUs []string
}

// QuotePricesOutput answers every asked SKU exactly once, in one of three
// ways, so a caller can tell a typo from a product that was taken off sale:
// Prices holds the active ones; Missing the SKUs no product exists for;
// Inactive the ones that exist but are deactivated. Missing and Inactive keep
// the order of the input.
type QuotePricesOutput struct {
	Prices   map[string]PriceOutput
	Missing  []string
	Inactive []string
}
