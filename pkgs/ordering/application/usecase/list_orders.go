package usecase

import (
	"context"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/mapper"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/ownership"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/pagination"
)

// Page-size policy of ListOrders. Clients ask, the server decides.
const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// ListOrders pages through orders newest first. The page token is minted and
// verified here, so every adapter (rpc, consumer, cli) shares one contract.
type ListOrders struct {
	repo order.Repository
	own  *ownership.Policy
}

func NewListOrders(repo order.Repository, own *ownership.Policy) *ListOrders {
	return &ListOrders{repo: repo, own: own}
}

// pageToken is the payload behind the opaque token. It carries the filters it
// was issued for, so a token replayed with other filters is rejected instead
// of silently resuming in the wrong key space.
type pageToken struct {
	CustomerID string    `json:"c"`
	Status     string    `json:"s"`
	CreatedAt  time.Time `json:"t"` // RFC 3339 with nanoseconds: lossless for Postgres microseconds
	ID         string    `json:"id"`
}

func (uc *ListOrders) Execute(c context.Context, in dto.ListOrdersInput) (*dto.ListOrdersOutput, error) {
	ctx := contextx.FromContext(c)

	limit, err := pagination.Clamp(in.PageSize, DefaultPageSize, MaxPageSize)
	if err != nil {
		return nil, err
	}

	params := order.ListParams{CustomerID: in.CustomerID, Limit: limit}
	if in.Status != "" {
		st, err := order.ParseStatus(in.Status)
		if err != nil {
			return nil, order.ErrInvalidStatusFilter
		}
		params.Status = &st
	}

	if in.PageToken != "" {
		var tok pageToken
		if err := pagination.DecodeToken(in.PageToken, &tok); err != nil {
			return nil, err
		}
		if tok.CustomerID != in.CustomerID || tok.Status != in.Status {
			return nil, pagination.ErrInvalidToken
		}
		cur, err := order.NewCursor(tok.CreatedAt, order.ID(tok.ID))
		if err != nil {
			return nil, pagination.ErrInvalidToken
		}
		params.After = &cur
	}

	orders, next, err := uc.repo.List(ctx, params)
	if err != nil {
		return nil, err
	}

	out := &dto.ListOrdersOutput{Orders: make([]dto.OrderOutput, 0, len(orders))}
	for _, o := range orders {
		out.Orders = append(out.Orders, *mapper.ToOrderOutput(o))
	}
	if next != nil {
		out.NextPageToken, err = pagination.EncodeToken(pageToken{
			CustomerID: in.CustomerID,
			Status:     in.Status,
			CreatedAt:  next.CreatedAt(),
			ID:         next.ID().String(),
		})
		if err != nil {
			return nil, err
		}
	}

	return out, nil
}
