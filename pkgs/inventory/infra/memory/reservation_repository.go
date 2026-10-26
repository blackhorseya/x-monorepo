package memory

import (
	"context"
	"sort"
	"sync"
	"time"

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

func (i *ReservationRepository) List(c context.Context, p reservation.ListParams) ([]*reservation.Reservation, *reservation.Cursor, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	matches := make([]*reservation.Reservation, 0, len(i.data))
	for _, r := range i.data {
		if p.Status != nil && r.Status() != *p.Status {
			continue
		}
		if p.After != nil && !p.After.Before(r.CreatedAt(), r.OrderID()) {
			continue
		}
		matches = append(matches, r)
	}
	sort.Slice(matches, func(a, b int) bool {
		x, y := matches[a], matches[b]
		if !x.CreatedAt().Equal(y.CreatedAt()) {
			return x.CreatedAt().After(y.CreatedAt())
		}
		return x.OrderID() > y.OrderID()
	})

	var next *reservation.Cursor
	if p.Limit > 0 && len(matches) > p.Limit {
		matches = matches[:p.Limit]
		cur := reservation.CursorOf(matches[p.Limit-1])
		next = &cur
	}
	out := make([]*reservation.Reservation, 0, len(matches))
	for _, r := range matches {
		cp, err := cloneReservation(r)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, cp)
	}

	return out, next, nil
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

// ListReservedBefore mirrors the Postgres query: RESERVED, hold started before
// the instant, oldest hold first (then order id, so the order is stable).
func (i *ReservationRepository) ListReservedBefore(c context.Context, before time.Time, limit int) ([]*reservation.Reservation, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	matches := make([]*reservation.Reservation, 0)
	for _, r := range i.data {
		if r.Status() == reservation.StatusReserved && r.UpdatedAt().Before(before) {
			matches = append(matches, r)
		}
	}
	sort.Slice(matches, func(a, b int) bool {
		x, y := matches[a], matches[b]
		if !x.UpdatedAt().Equal(y.UpdatedAt()) {
			return x.UpdatedAt().Before(y.UpdatedAt())
		}
		return x.OrderID() < y.OrderID()
	})
	if limit > 0 && len(matches) > limit {
		matches = matches[:limit]
	}
	out := make([]*reservation.Reservation, 0, len(matches))
	for _, r := range matches {
		cp, err := cloneReservation(r)
		if err != nil {
			return nil, err
		}
		out = append(out, cp)
	}

	return out, nil
}

// CountByStatus mirrors the Postgres GROUP BY: statuses with no reservation are absent.
func (i *ReservationRepository) CountByStatus(c context.Context) ([]reservation.StatusCount, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	type acc struct {
		n      int64
		oldest time.Time
	}
	by := map[reservation.Status]*acc{}
	for _, r := range i.data {
		a := by[r.Status()]
		if a == nil {
			a = &acc{oldest: r.UpdatedAt()}
			by[r.Status()] = a
		}
		a.n++
		if r.UpdatedAt().Before(a.oldest) {
			a.oldest = r.UpdatedAt()
		}
	}

	out := make([]reservation.StatusCount, 0, len(by))
	for _, st := range reservation.Statuses() {
		if a, ok := by[st]; ok {
			out = append(out, reservation.NewStatusCount(st, a.n, a.oldest))
		}
	}

	return out, nil
}
