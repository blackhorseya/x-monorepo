// Package mapper converts domain objects to output DTOs (domain → dto only;
// dto → domain happens inside use cases because it involves validation).
package mapper

import (
	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
)

func ToOrderOutput(o *order.Order) *dto.OrderOutput {
	items := o.Items()
	out := &dto.OrderOutput{
		ID:         o.ID().String(),
		CustomerID: o.CustomerID(),
		Items:      make([]dto.LineItem, 0, len(items)),
		Total:      toMoney(o.Total()),
		Status:     o.Status().String(),
		CreatedAt:  o.CreatedAt(),
		UpdatedAt:  o.UpdatedAt(),
	}
	for _, it := range items {
		out.Items = append(out.Items, dto.LineItem{
			SKU:       it.SKU(),
			Quantity:  it.Quantity(),
			UnitPrice: toMoney(it.UnitPrice()),
		})
	}

	return out
}

func toMoney(m kernel.Money) dto.Money {
	return dto.Money{AmountMinor: m.Amount(), Currency: m.Currency()}
}
