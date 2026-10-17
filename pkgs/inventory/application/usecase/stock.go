package usecase

import (
	"context"
	"errors"
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/mapper"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/stock"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// SetStock sets the on-hand count of a SKU, creating it on first use.
type SetStock struct {
	stocks stock.Repository
	tx     port.Transactor
	clock  port.Clock
}

func NewSetStock(stocks stock.Repository, tx port.Transactor, clock port.Clock) *SetStock {
	return &SetStock{stocks: stocks, tx: tx, clock: clock}
}

func (uc *SetStock) Execute(c context.Context, in dto.SetStockInput) (*dto.StockOutput, error) {
	ctx := contextx.FromContext(c)

	var s *stock.Stock
	err := uc.tx.Do(ctx, func(c context.Context) error {
		var err error
		s, err = uc.stocks.FindBySKU(c, stock.SKU(in.SKU))
		switch {
		case errors.Is(err, stock.ErrNotFound):
			if s, err = stock.New(stock.SKU(in.SKU), in.OnHand, uc.clock.Now()); err != nil {
				return err
			}
		case err != nil:
			return fmt.Errorf("find stock %s: %w", in.SKU, err)
		default:
			if err = s.Restock(in.OnHand, uc.clock.Now()); err != nil {
				return err
			}
		}
		if err := uc.stocks.Save(c, s); err != nil {
			return fmt.Errorf("save stock %s: %w", in.SKU, err)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return mapper.ToStockOutput(s), nil
}

// GetStock reads one SKU.
type GetStock struct {
	stocks stock.Repository
}

func NewGetStock(stocks stock.Repository) *GetStock { return &GetStock{stocks: stocks} }

func (uc *GetStock) Execute(c context.Context, in dto.GetStockInput) (*dto.StockOutput, error) {
	if in.SKU == "" {
		return nil, stock.ErrInvalidSKU
	}
	s, err := uc.stocks.FindBySKU(contextx.FromContext(c), stock.SKU(in.SKU))
	if err != nil {
		return nil, err
	}

	return mapper.ToStockOutput(s), nil
}

// GetReservation reads what inventory recorded for an order.
type GetReservation struct {
	reservations reservation.Repository
}

func NewGetReservation(reservations reservation.Repository) *GetReservation {
	return &GetReservation{reservations: reservations}
}

func (uc *GetReservation) Execute(c context.Context, in dto.GetReservationInput) (*dto.ReservationOutput, error) {
	if in.OrderID == "" {
		return nil, reservation.ErrInvalidOrderID
	}
	r, err := uc.reservations.FindByOrderID(contextx.FromContext(c), in.OrderID)
	if err != nil {
		return nil, err
	}

	return mapper.ToReservationOutput(r), nil
}
