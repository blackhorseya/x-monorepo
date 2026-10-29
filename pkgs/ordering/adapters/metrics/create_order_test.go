package metrics_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/metrics"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// The decorator maps what the use case returned to one result label, and
// nothing else: the only attribute is result, and unrelated failures are not counted.
func TestCreateOrderResults(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	results, err := metrics.NewCreateOrderResults(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	if err != nil {
		t.Fatal(err)
	}

	var next func() (*dto.OrderOutput, error)
	uc := usecasex.Chain(
		usecasex.Func[dto.CreateOrderInput, *dto.OrderOutput](func(context.Context, dto.CreateOrderInput) (*dto.OrderOutput, error) {
			return next()
		}),
		results.Decorator(),
	)
	run := func(out *dto.OrderOutput, err error, times int) {
		next = func() (*dto.OrderOutput, error) { return out, err }
		for range times {
			// The decorator must hand the use case's own answer back untouched.
			gotOut, gotErr := uc.Execute(context.Background(), dto.CreateOrderInput{IdempotencyKey: "never-a-label", CustomerID: "never-a-label"})
			if gotOut != out || !errors.Is(gotErr, err) {
				t.Fatalf("Execute = %v, %v; want %v, %v", gotOut, gotErr, out, err)
			}
		}
	}

	run(&dto.OrderOutput{ID: "o-1"}, nil, 3)
	run(&dto.OrderOutput{ID: "o-1", Replayed: true}, nil, 2)
	run(nil, fmt.Errorf("create: %w", order.ErrIdempotencyKeyReused), 1)
	run(nil, order.ErrEmptyCustomer, 4)           // validation: not counted here
	run(nil, errors.New("commit: conn reset"), 5) // infrastructure: not counted here

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "ordering.create_order" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				if dp.Attributes.Len() != 1 {
					t.Fatalf("attributes = %v, want only result", dp.Attributes.ToSlice())
				}
				v, _ := dp.Attributes.Value("result")
				got[v.AsString()] = dp.Value
			}
		}
	}
	want := map[string]int64{metrics.ResultCreated: 3, metrics.ResultReplayed: 2, metrics.ResultKeyReused: 1}
	if len(got) != len(want) {
		t.Fatalf("results = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("results = %v, want %v", got, want)
		}
	}
}
