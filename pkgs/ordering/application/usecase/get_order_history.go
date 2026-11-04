package usecase

import (
	"context"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/mapper"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/ownership"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// GetOrderHistory returns the transitions of one order.
type GetOrderHistory struct {
	repo    order.Repository
	history port.HistoryStore
	own     *ownership.Policy
}

func NewGetOrderHistory(repo order.Repository, history port.HistoryStore, own *ownership.Policy) *GetOrderHistory {
	return &GetOrderHistory{repo: repo, history: history, own: own}
}

func (uc *GetOrderHistory) Execute(c context.Context, in dto.GetOrderHistoryInput) (*dto.GetOrderHistoryOutput, error) {
	ctx := contextx.FromContext(c)

	if in.OrderID == "" {
		return nil, order.ErrInvalidID
	}
	if _, err := uc.repo.FindByID(ctx, order.ID(in.OrderID)); err != nil {
		return nil, err
	}
	entries, err := uc.history.List(ctx, order.ID(in.OrderID))
	if err != nil {
		return nil, err
	}

	out := &dto.GetOrderHistoryOutput{Entries: make([]dto.HistoryEntryOutput, 0, len(entries))}
	for _, e := range entries {
		out.Entries = append(out.Entries, mapper.ToHistoryOutput(e, true))
	}

	return out, nil
}
