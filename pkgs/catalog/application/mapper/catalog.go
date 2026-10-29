// Package mapper turns domain aggregates into application DTOs.
package mapper

import (
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/domain/product"
	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

func ToPriceOutput(m kernel.Money) dto.PriceOutput {
	return dto.PriceOutput{Amount: m.Amount(), Currency: m.Currency()}
}

func ToProductOutput(p *product.Product) *dto.ProductOutput {
	return &dto.ProductOutput{
		SKU:        p.SKU().String(),
		Name:       p.Name(),
		Price:      ToPriceOutput(p.Price()),
		Active:     p.Active(),
		UpdateTime: p.UpdatedAt(),
	}
}
