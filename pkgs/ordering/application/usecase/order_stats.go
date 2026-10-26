package usecase

import (
	"context"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// OrderStats counts orders per status for the worker's funnel gauges
// (adapters/metrics). Every status order.Statuses knows is in the output, zero
// when the repository has none, so a series never disappears when a status
// empties out — an alert on "PENDING for too long" must see 0, not no data.
type OrderStats struct {
	repo order.Repository
}

func NewOrderStats(repo order.Repository) *OrderStats { return &OrderStats{repo: repo} }

func (uc *OrderStats) Execute(c context.Context, _ dto.OrderStatsInput) (*dto.OrderStatsOutput, error) {
	ctx := contextx.FromContext(c)

	counts, err := uc.repo.CountByStatus(ctx)
	if err != nil {
		return nil, err
	}
	byStatus := make(map[order.Status]order.StatusCount, len(counts))
	for _, sc := range counts {
		byStatus[sc.Status()] = sc
	}

	out := &dto.OrderStatsOutput{}
	for _, st := range order.Statuses() {
		sc := byStatus[st]
		out.Statuses = append(out.Statuses, dto.StatusStat{Status: st.String(), Count: sc.Count(), OldestUpdatedAt: sc.OldestUpdatedAt()})
	}

	return out, nil
}
