package memory

import (
	"context"
	"sync"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/port"
)

var _ port.PriceQuoter = (*PriceList)(nil)

// PriceList is a port.PriceQuoter over a fixed list of prices, for tests and for composing
// ordering without a catalog: it answers like the catalog does (a price, or missing, or
// inactive), counts the questions it was asked, and can be made to fail.
type PriceList struct {
	mu       sync.Mutex
	prices   map[string]dto.Money
	inactive map[string]struct{}
	flat     *dto.Money // every SKU not listed is priced like this
	calls    int
	last     []string
	err      error
}

// NewPriceList is an empty list: every SKU is missing until Set.
func NewPriceList() *PriceList {
	return &PriceList{prices: map[string]dto.Money{}, inactive: map[string]struct{}{}}
}

// NewFlatPriceList prices every SKU the same, unless it was Set to something else or
// Deactivated: for tests that order but do not care what things cost.
func NewFlatPriceList(amountMinor int64, currency string) *PriceList {
	l := NewPriceList()
	l.flat = &dto.Money{AmountMinor: amountMinor, Currency: currency}

	return l
}

// Set prices sku (and puts it on sale again).
func (i *PriceList) Set(sku string, amountMinor int64, currency string) *PriceList {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.prices[sku] = dto.Money{AmountMinor: amountMinor, Currency: currency}
	delete(i.inactive, sku)

	return i
}

// Deactivate takes sku off sale: it is quoted as inactive from now on.
func (i *PriceList) Deactivate(sku string) *PriceList {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.inactive[sku] = struct{}{}

	return i
}

// FailWith makes every following QuotePrices return err.
func (i *PriceList) FailWith(err error) *PriceList {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.err = err

	return i
}

// Calls is how many times the list was asked.
func (i *PriceList) Calls() int {
	i.mu.Lock()
	defer i.mu.Unlock()

	return i.calls
}

// LastSKUs is what the latest question asked for.
func (i *PriceList) LastSKUs() []string {
	i.mu.Lock()
	defer i.mu.Unlock()

	return append([]string(nil), i.last...)
}

func (i *PriceList) QuotePrices(c context.Context, skus []string) (*dto.PriceQuote, error) {
	if err := c.Err(); err != nil {
		return nil, err
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls++
	i.last = append([]string(nil), skus...)
	if i.err != nil {
		return nil, i.err
	}

	out := &dto.PriceQuote{Prices: make(map[string]dto.Money, len(skus))}
	for _, sku := range skus {
		price, listed := i.prices[sku]
		switch _, off := i.inactive[sku]; {
		case off:
			out.Inactive = append(out.Inactive, sku)
		case listed:
			out.Prices[sku] = price
		case i.flat != nil:
			out.Prices[sku] = *i.flat
		default:
			out.Missing = append(out.Missing, sku)
		}
	}

	return out, nil
}
