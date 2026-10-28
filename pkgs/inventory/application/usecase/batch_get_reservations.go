package usecase

import (
	"context"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/mapper"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// MaxBatchOrderIDs bounds one BatchGetReservations call (the same as a page).
const MaxBatchOrderIDs = 100

// BatchGetReservations is GetReservation for many orders in one read (the
// console's orders list asks for a page of them each tick). An order without
// a reservation is left out of the result instead of failing the call; a
// repeated id is answered once.
type BatchGetReservations struct {
	reservations reservation.Repository
}

func NewBatchGetReservations(reservations reservation.Repository) *BatchGetReservations {
	return &BatchGetReservations{reservations: reservations}
}

func (uc *BatchGetReservations) Execute(c context.Context, in dto.BatchGetReservationsInput) (*dto.BatchGetReservationsOutput, error) {
	if len(in.OrderIDs) == 0 {
		return nil, reservation.ErrInvalidOrderID
	}
	if len(in.OrderIDs) > MaxBatchOrderIDs {
		return nil, reservation.ErrTooManyOrderIDs
	}
	seen := make(map[string]struct{}, len(in.OrderIDs))
	ids := make([]string, 0, len(in.OrderIDs))
	for _, id := range in.OrderIDs {
		if id == "" {
			return nil, reservation.ErrInvalidOrderID
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	rs, err := uc.reservations.FindByOrderIDs(contextx.FromContext(c), ids)
	if err != nil {
		return nil, err
	}
	out := &dto.BatchGetReservationsOutput{Reservations: make([]dto.ReservationOutput, 0, len(rs))}
	for _, r := range rs {
		out.Reservations = append(out.Reservations, *mapper.ToReservationOutput(r))
	}

	return out, nil
}
