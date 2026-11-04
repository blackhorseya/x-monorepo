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

// GetOrderHistory returns the transitions of one order, oldest first.
//
// Whose order it is is decided where the order is selected, exactly as GetOrder does: a confined
// caller's owner condition goes into the lookup (FindByIDForCustomer), so somebody else's order is
// ErrNotFound, the answer for a missing one, and the history table is not read for it at all. An
// order that exists but has no history (it predates the table) is an empty list.
//
// Who acted (actor_subject) is cleared here for everyone but an admin, so the answer the adapters map
// never contains it.
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
	find := func() (*order.Order, error) { return uc.repo.FindByID(ctx, order.ID(in.OrderID)) }
	if customer, confined := uc.own.OwnOnly(ctx); confined {
		find = func() (*order.Order, error) { return uc.repo.FindByIDForCustomer(ctx, order.ID(in.OrderID), customer) }
	}
	if _, err := find(); err != nil {
		return nil, err
	}
	entries, err := uc.history.List(ctx, order.ID(in.OrderID))
	if err != nil {
		return nil, err
	}

	showSubject := uc.own.IsAdmin(ctx)
	out := &dto.GetOrderHistoryOutput{Entries: make([]dto.HistoryEntryOutput, 0, len(entries))}
	for _, e := range entries {
		out.Entries = append(out.Entries, mapper.ToHistoryOutput(e, showSubject))
	}

	return out, nil
}
