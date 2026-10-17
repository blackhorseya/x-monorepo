package memory

import (
	"context"
	"sync"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

var _ reservation.Repository = (*ReservationRepository)(nil)

type ReservationRepository struct {
	mu   sync.RWMutex
	data map[string]*reservation.Reservation
}

func NewReservationRepository() *ReservationRepository {
	return &ReservationRepository{data: make(map[string]*reservation.Reservation)}
}

func (i *ReservationRepository) FindByOrderID(c context.Context, orderID string) (*reservation.Reservation, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	r, ok := i.data[orderID]
	if !ok {
		return nil, reservation.ErrNotFound
	}

	return cloneReservation(r)
}

func (i *ReservationRepository) Add(c context.Context, r *reservation.Reservation) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	cp, err := cloneReservation(r)
	if err != nil {
		return err
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	if _, exists := i.data[r.OrderID()]; exists {
		return reservation.ErrDuplicate
	}
	i.data[r.OrderID()] = cp

	return nil
}

func (i *ReservationRepository) Update(c context.Context, r *reservation.Reservation) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	cp, err := cloneReservation(r)
	if err != nil {
		return err
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	if _, exists := i.data[r.OrderID()]; !exists {
		return reservation.ErrNotFound
	}
	i.data[r.OrderID()] = cp

	return nil
}

func cloneReservation(r *reservation.Reservation) (*reservation.Reservation, error) {
	return reservation.Reconstitute(reservation.ReconstituteParams{
		OrderID:   r.OrderID(),
		Lines:     r.Lines(),
		Status:    r.Status(),
		Shortfall: r.Shortfall(),
		CreatedAt: r.CreatedAt(),
		UpdatedAt: r.UpdatedAt(),
	})
}
