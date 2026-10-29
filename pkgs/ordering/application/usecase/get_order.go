package usecase

import (
	"context"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/mapper"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/ownership"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// GetOrder loads a single order.
type GetOrder struct {
	repo order.Repository
	own  *ownership.Policy
}

func NewGetOrder(repo order.Repository, own *ownership.Policy) *GetOrder {
	return &GetOrder{repo: repo, own: own}
}

func (uc *GetOrder) Execute(c context.Context, in dto.GetOrderInput) (*dto.OrderOutput, error) {
	ctx := contextx.FromContext(c)

	if in.OrderID == "" {
		return nil, order.ErrInvalidID
	}

	o, err := uc.repo.FindByID(ctx, order.ID(in.OrderID))
	if err != nil {
		return nil, err
	}

	return mapper.ToOrderOutput(o), nil
}
