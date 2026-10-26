package usecase

import (
	"context"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// ReservationStats counts reservations per status for the worker's funnel
// gauges (adapters/metrics). Every status reservation.Statuses knows is in the
// output, zero when the repository has none, so a series never disappears.
type ReservationStats struct {
	repo reservation.Repository
}

func NewReservationStats(repo reservation.Repository) *ReservationStats {
	return &ReservationStats{repo: repo}
}

func (uc *ReservationStats) Execute(c context.Context, _ dto.ReservationStatsInput) (*dto.ReservationStatsOutput, error) {
	ctx := contextx.FromContext(c)

	counts, err := uc.repo.CountByStatus(ctx)
	if err != nil {
		return nil, err
	}
	byStatus := make(map[reservation.Status]reservation.StatusCount, len(counts))
	for _, sc := range counts {
		byStatus[sc.Status()] = sc
	}

	out := &dto.ReservationStatsOutput{}
	for _, st := range reservation.Statuses() {
		sc := byStatus[st]
		out.Statuses = append(out.Statuses, dto.StatusStat{Status: st.String(), Count: sc.Count(), OldestUpdatedAt: sc.OldestUpdatedAt()})
	}

	return out, nil
}
