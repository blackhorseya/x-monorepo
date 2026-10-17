// Package mapper converts domain objects to output DTOs (domain → dto only;
// dto → domain happens inside use cases because it involves validation).
package mapper

import (
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/stock"
)

func ToStockOutput(s *stock.Stock) *dto.StockOutput {
	return &dto.StockOutput{
		SKU:       s.SKU().String(),
		OnHand:    s.OnHand(),
		Reserved:  s.Reserved(),
		Available: s.Available(),
		UpdatedAt: s.UpdatedAt(),
	}
}

func ToReservationOutput(r *reservation.Reservation) *dto.ReservationOutput {
	lines := r.Lines()
	out := &dto.ReservationOutput{
		OrderID:       r.OrderID(),
		Lines:         make([]dto.Line, 0, len(lines)),
		Status:        r.Status().String(),
		ShortfallSKUs: r.Shortfall(),
		CreatedAt:     r.CreatedAt(),
		UpdatedAt:     r.UpdatedAt(),
	}
	for _, l := range lines {
		out.Lines = append(out.Lines, dto.Line{SKU: l.SKU(), Quantity: l.Quantity()})
	}

	return out
}
