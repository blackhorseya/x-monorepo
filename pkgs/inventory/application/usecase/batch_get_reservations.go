package usecase

import (
	"context"
	"errors"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
)

// MaxBatchOrderIDs bounds one BatchGetReservations call (the same as a page).
const MaxBatchOrderIDs = 100

// BatchGetReservations is GetReservation for many orders in one read.
type BatchGetReservations struct {
	reservations reservation.Repository
}

func NewBatchGetReservations(reservations reservation.Repository) *BatchGetReservations {
	return &BatchGetReservations{reservations: reservations}
}

func (uc *BatchGetReservations) Execute(c context.Context, in dto.BatchGetReservationsInput) (*dto.BatchGetReservationsOutput, error) {
	return nil, errors.New("not implemented")
}
