package usecase

import (
	"context"

	"github.com/blackhorseya/x-monorepo/pkgs/catalog/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/application/mapper"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/domain/product"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/pagination"
)

// Page-size policy of ListProducts. Clients ask, the server decides.
const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// ListProducts pages through SKUs ascending, optionally by prefix. The page
// token is minted and verified here (platform/pagination), so every adapter
// shares one contract; it carries the filter it was minted with, so a page
// cannot be continued under another.
type ListProducts struct {
	products product.Repository
}

func NewListProducts(products product.Repository) *ListProducts {
	return &ListProducts{products: products}
}

// pageToken is the payload behind the opaque token: the last SKU of the page
// and the filter that produced it.
type pageToken struct {
	SKU    string `json:"sku"`
	Prefix string `json:"p,omitempty"`
}

func (uc *ListProducts) Execute(c context.Context, in dto.ListProductsInput) (*dto.ListProductsOutput, error) {
	ctx := contextx.FromContext(c)

	limit, err := pagination.Clamp(in.PageSize, DefaultPageSize, MaxPageSize)
	if err != nil {
		return nil, err
	}
	params := product.ListParams{Limit: limit, SKUPrefix: in.SKUPrefix}
	if in.PageToken != "" {
		var tok pageToken
		if err := pagination.DecodeToken(in.PageToken, &tok); err != nil {
			return nil, err
		}
		if tok.SKU == "" || tok.Prefix != in.SKUPrefix {
			return nil, pagination.ErrInvalidToken
		}
		params.After = product.SKU(tok.SKU)
	}

	products, next, err := uc.products.List(ctx, params)
	if err != nil {
		return nil, err
	}
	out := &dto.ListProductsOutput{Products: make([]dto.ProductOutput, 0, len(products))}
	for _, p := range products {
		out.Products = append(out.Products, *mapper.ToProductOutput(p))
	}
	if next != "" {
		if out.NextPageToken, err = pagination.EncodeToken(pageToken{SKU: next.String(), Prefix: in.SKUPrefix}); err != nil {
			return nil, err
		}
	}

	return out, nil
}
