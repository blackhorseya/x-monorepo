// Package metrics is the driving adapter that turns the ReservationStats use
// case into observable gauges — inventory's half of the saga funnel:
//
//	inventory_reservations{status}                    how many reservations sit in each status
//	inventory_reservations_oldest_age_seconds{status} how long the oldest of them has been there
//
// RESERVED growing old is stock held for an order that never moved on (the
// reservation TTL is the next step on the roadmap). Like eventbus.Lag these
// are read from the database on every scrape by every worker replica, so
// aggregate with max, never sum. apps/worker registers them (it is the process
// with the database and the lag gauges); rpc does not.
package metrics

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

type ReservationStatsUC = usecasex.UseCase[dto.ReservationStatsInput, *dto.ReservationStatsOutput]

// Gauges holds the use case and knows how to register the two instruments.
type Gauges struct {
	stats ReservationStatsUC
	now   func() time.Time
}

func NewGauges(stats ReservationStatsUC) *Gauges { return &Gauges{stats: stats, now: time.Now} }

// Register adds the instruments to mp and returns the callback registration;
// unregister it when the process stops reporting.
func (h *Gauges) Register(mp metric.MeterProvider) (metric.Registration, error) {
	meter := mp.Meter("github.com/blackhorseya/x-monorepo/pkgs/inventory/adapters/metrics")
	count, err := meter.Int64ObservableGauge("inventory.reservations",
		metric.WithDescription("Reservations per status (the saga funnel; per replica: aggregate with max)"),
		metric.WithUnit("{reservation}"))
	if err != nil {
		return nil, err
	}
	age, err := meter.Float64ObservableGauge("inventory.reservations.oldest_age",
		metric.WithDescription("Age of the oldest reservation in each status since its last status change (0 when none)"),
		metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}

	return meter.RegisterCallback(func(c context.Context, o metric.Observer) error {
		c, cancel := context.WithTimeout(c, 5*time.Second)
		defer cancel()
		out, err := h.stats.Execute(c, dto.ReservationStatsInput{})
		if err != nil {
			return err
		}
		now := h.now()
		for _, s := range out.Statuses {
			attrs := metric.WithAttributes(attribute.String("status", s.Status))
			o.ObserveInt64(count, s.Count, attrs)
			var secs float64
			if s.Count > 0 {
				secs = now.Sub(s.OldestUpdatedAt).Seconds()
			}
			o.ObserveFloat64(age, secs, attrs)
		}
		return nil
	}, count, age)
}
