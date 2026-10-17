package usecasex_test

import (
	"context"
	"errors"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

func TestMetrics_KindAttribute(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	rec, err := usecasex.NewRecorder(mp)
	if err != nil {
		t.Fatal(err)
	}

	var fail error
	uc := usecasex.Chain[int, int](
		usecasex.Func[int, int](func(_ context.Context, in int) (int, error) { return in, fail }),
		usecasex.Metrics[int, int](rec, "test.echo"),
	)

	c := context.Background()
	_, _ = uc.Execute(c, 1) // ok
	fail = kernel.NewError(kernel.KindNotFound, "TEST_NOT_FOUND", "nope")
	_, _ = uc.Execute(c, 1) // not_found
	_, _ = uc.Execute(c, 1) // not_found
	fail = errors.New("boom")
	_, _ = uc.Execute(c, 1) // internal

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(c, &rm); err != nil {
		t.Fatal(err)
	}

	got := map[string]uint64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "usecase.duration" {
				continue
			}
			if m.Unit != "s" {
				t.Fatalf("unit = %q, want s", m.Unit)
			}
			hist, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("data type = %T, want Histogram[float64]", m.Data)
			}
			for _, dp := range hist.DataPoints {
				uc, _ := dp.Attributes.Value("usecase")
				if uc.AsString() != "test.echo" {
					t.Fatalf("usecase attribute = %q", uc.AsString())
				}
				kind, _ := dp.Attributes.Value("kind")
				got[kind.AsString()] = dp.Count
			}
		}
	}

	want := map[string]uint64{"ok": 1, "not_found": 2, "internal": 1}
	for k, n := range want {
		if got[k] != n {
			t.Fatalf("kind %q count = %d, want %d (all: %v)", k, got[k], n, got)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("unexpected kinds: %v", got)
	}
}
