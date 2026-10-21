package usecase

import (
	"context"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/mapper"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/pagination"
)

// ListReservations pages through reservations newest first, optionally by
// status. The page token is minted and verified here (platform/pagination)
// and carries the filter it was issued for, so a token replayed with another
// filter is rejected instead of silently resuming in the wrong key space.
type ListReservations struct {
	reservations reservation.Repository
}

func NewListReservations(reservations reservation.Repository) *ListReservations {
	return &ListReservations{reservations: reservations}
}

// reservationPageToken is the payload behind the opaque token.
type reservationPageToken struct {
	Status    string    `json:"s"`
	CreatedAt time.Time `json:"t"` // RFC 3339 with nanoseconds: lossless for Postgres microseconds
	OrderID   string    `json:"o"`
}

func (uc *ListReservations) Execute(c context.Context, in dto.ListReservationsInput) (*dto.ListReservationsOutput, error) {
	ctx := contextx.FromContext(c)

	limit, err := pagination.Clamp(in.PageSize, DefaultPageSize, MaxPageSize)
	if err != nil {
		return nil, err
	}
	params := reservation.ListParams{Limit: limit}
	if in.Status != "" {
		st, err := reservation.ParseStatus(in.Status)
		if err != nil {
			return nil, reservation.ErrInvalidStatusFilter
		}
		params.Status = &st
	}
	if in.PageToken != "" {
		var tok reservationPageToken
		if err := pagination.DecodeToken(in.PageToken, &tok); err != nil {
			return nil, err
		}
		if tok.Status != in.Status {
			return nil, pagination.ErrInvalidToken
		}
		cur, err := reservation.NewCursor(tok.CreatedAt, tok.OrderID)
		if err != nil {
			return nil, pagination.ErrInvalidToken
		}
		params.After = &cur
	}

	rs, next, err := uc.reservations.List(ctx, params)
	if err != nil {
		return nil, err
	}
	out := &dto.ListReservationsOutput{Reservations: make([]dto.ReservationOutput, 0, len(rs))}
	for _, r := range rs {
		out.Reservations = append(out.Reservations, *mapper.ToReservationOutput(r))
	}
	if next != nil {
		out.NextPageToken, err = pagination.EncodeToken(reservationPageToken{Status: in.Status, CreatedAt: next.CreatedAt(), OrderID: next.OrderID()})
		if err != nil {
			return nil, err
		}
	}

	return out, nil
}
