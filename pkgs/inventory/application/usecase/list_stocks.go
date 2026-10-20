package usecase

import (
	"context"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/mapper"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/stock"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/pagination"
)

// Page-size policy of ListStocks. Clients ask, the server decides.
const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// ListStocks pages through every SKU ascending. The page token is minted and
// verified here (platform/pagination), so every adapter shares one contract.
type ListStocks struct {
	stocks stock.Repository
}

func NewListStocks(stocks stock.Repository) *ListStocks { return &ListStocks{stocks: stocks} }

// pageToken is the payload behind the opaque token: the last SKU of the page.
type pageToken struct {
	SKU string `json:"sku"`
}

func (uc *ListStocks) Execute(c context.Context, in dto.ListStocksInput) (*dto.ListStocksOutput, error) {
	ctx := contextx.FromContext(c)

	limit, err := pagination.Clamp(in.PageSize, DefaultPageSize, MaxPageSize)
	if err != nil {
		return nil, err
	}
	params := stock.ListParams{Limit: limit}
	if in.PageToken != "" {
		var tok pageToken
		if err := pagination.DecodeToken(in.PageToken, &tok); err != nil {
			return nil, err
		}
		if tok.SKU == "" {
			return nil, pagination.ErrInvalidToken
		}
		params.After = stock.SKU(tok.SKU)
	}

	stocks, next, err := uc.stocks.List(ctx, params)
	if err != nil {
		return nil, err
	}
	out := &dto.ListStocksOutput{Stocks: make([]dto.StockOutput, 0, len(stocks))}
	for _, s := range stocks {
		out.Stocks = append(out.Stocks, *mapper.ToStockOutput(s))
	}
	if next != "" {
		if out.NextPageToken, err = pagination.EncodeToken(pageToken{SKU: next.String()}); err != nil {
			return nil, err
		}
	}

	return out, nil
}
