// Package usecase holds the catalog use cases: orchestration only, the rules
// live in domain/product.
package usecase

import (
	"context"
	"errors"
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/catalog/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/application/mapper"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/domain/product"
	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// UpsertProduct creates a product, or replaces the name and price of an
// existing one. An inactive product is activated again: the catalog has no
// other way back onto the shelf.
type UpsertProduct struct {
	products product.Repository
	clock    port.Clock
}

func NewUpsertProduct(products product.Repository, clock port.Clock) *UpsertProduct {
	return &UpsertProduct{products: products, clock: clock}
}

func (uc *UpsertProduct) Execute(c context.Context, in dto.UpsertProductInput) (*dto.ProductOutput, error) {
	ctx := contextx.FromContext(c)

	price, err := kernel.NewMoney(in.Amount, in.Currency)
	if err != nil {
		return nil, err
	}
	now := uc.clock.Now()

	p, err := uc.products.FindBySKU(ctx, product.SKU(in.SKU))
	switch {
	case errors.Is(err, product.ErrNotFound):
		if p, err = product.New(product.SKU(in.SKU), in.Name, price, now); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, fmt.Errorf("find product %s: %w", in.SKU, err)
	default:
		if err = p.Rename(in.Name, now); err != nil {
			return nil, err
		}
		if err = p.Reprice(price, now); err != nil {
			return nil, err
		}
		p.Activate(now)
	}
	if err = uc.products.Save(ctx, p); err != nil {
		return nil, fmt.Errorf("save product %s: %w", in.SKU, err)
	}

	return mapper.ToProductOutput(p), nil
}

// GetProduct reads one SKU, active or not.
type GetProduct struct {
	products product.Repository
}

func NewGetProduct(products product.Repository) *GetProduct { return &GetProduct{products: products} }

func (uc *GetProduct) Execute(c context.Context, in dto.GetProductInput) (*dto.ProductOutput, error) {
	if in.SKU == "" {
		return nil, product.ErrInvalidSKU
	}
	p, err := uc.products.FindBySKU(contextx.FromContext(c), product.SKU(in.SKU))
	if err != nil {
		return nil, err
	}

	return mapper.ToProductOutput(p), nil
}

// DeactivateProduct takes a product off sale. Doing it twice is not an error.
type DeactivateProduct struct {
	products product.Repository
	clock    port.Clock
}

func NewDeactivateProduct(products product.Repository, clock port.Clock) *DeactivateProduct {
	return &DeactivateProduct{products: products, clock: clock}
}

func (uc *DeactivateProduct) Execute(c context.Context, in dto.DeactivateProductInput) (*dto.ProductOutput, error) {
	ctx := contextx.FromContext(c)
	if in.SKU == "" {
		return nil, product.ErrInvalidSKU
	}

	p, err := uc.products.FindBySKU(ctx, product.SKU(in.SKU))
	if err != nil {
		return nil, err
	}
	if !p.Active() {
		return mapper.ToProductOutput(p), nil
	}
	p.Deactivate(uc.clock.Now())
	if err = uc.products.Save(ctx, p); err != nil {
		return nil, fmt.Errorf("save product %s: %w", in.SKU, err)
	}

	return mapper.ToProductOutput(p), nil
}
