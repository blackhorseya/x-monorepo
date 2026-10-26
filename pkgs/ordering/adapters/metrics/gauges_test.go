package metrics_test

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/metrics"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// One gauge per status for the count, one for the age of the oldest; an empty
// status reports 0 for both instead of disappearing.
func TestGauges(t *testing.T) {
	old := time.Now().Add(-10 * time.Minute)
	stats := usecasex.Func[dto.OrderStatsInput, *dto.OrderStatsOutput](func(context.Context, dto.OrderStatsInput) (*dto.OrderStatsOutput, error) {
		return &dto.OrderStatsOutput{Statuses: []dto.StatusStat{
			{Status: "PENDING", Count: 2, OldestUpdatedAt: old},
			{Status: "CONFIRMED"},
		}}, nil
	})
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	reg, err := metrics.NewGauges(stats).Register(mp)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Unregister() }()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int64{}
	ages := map[string]float64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "ordering.orders":
				for _, dp := range m.Data.(metricdata.Gauge[int64]).DataPoints {
					counts[status(dp.Attributes)] = dp.Value
				}
			case "ordering.orders.oldest_age":
				if m.Unit != "s" {
					t.Fatalf("age unit = %q, want s", m.Unit)
				}
				for _, dp := range m.Data.(metricdata.Gauge[float64]).DataPoints {
					ages[status(dp.Attributes)] = dp.Value
				}
			}
		}
	}
	if counts["PENDING"] != 2 || counts["CONFIRMED"] != 0 || len(counts) != 2 {
		t.Fatalf("counts = %v", counts)
	}
	if ages["PENDING"] < 590 || ages["PENDING"] > 700 || ages["CONFIRMED"] != 0 {
		t.Fatalf("ages = %v", ages)
	}
}

func status(set attribute.Set) string {
	v, _ := set.Value("status")
	return v.AsString()
}
