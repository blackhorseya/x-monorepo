package metrics_test

import (
	"context"
	"errors"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/metrics"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// ordering_payment_event_total{event,effect}: what each payment event did, counted after the use case
// returned (a failure is not counted), with no order or customer in any label. "unexpected" is the series
// an alert reads (a PaymentCaptured for an order that never owed payment).
func TestPaymentEventResults(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	results, err := metrics.NewPaymentEventResults(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	if err != nil {
		t.Fatal(err)
	}

	var next func() (*dto.PaymentEventOutput, error)
	captured := usecasex.Chain(
		usecasex.Func[dto.PaymentCapturedInput, *dto.PaymentEventOutput](func(context.Context, dto.PaymentCapturedInput) (*dto.PaymentEventOutput, error) { return next() }),
		results.Captured(),
	)
	expired := usecasex.Chain(
		usecasex.Func[dto.PaymentExpiredInput, *dto.PaymentEventOutput](func(context.Context, dto.PaymentExpiredInput) (*dto.PaymentEventOutput, error) { return next() }),
		results.Expired(),
	)
	runCaptured := func(effect string, err error, times int) {
		out := &dto.PaymentEventOutput{Effect: effect}
		next = func() (*dto.PaymentEventOutput, error) { return out, err }
		for range times {
			if got, gerr := captured.Execute(context.Background(), dto.PaymentCapturedInput{OrderID: "never-a-label"}); got != out || !errors.Is(gerr, err) {
				t.Fatalf("Execute = %v, %v; want the use case's own answer", got, gerr)
			}
		}
	}
	runExpired := func(effect string, times int) {
		next = func() (*dto.PaymentEventOutput, error) { return &dto.PaymentEventOutput{Effect: effect}, nil }
		for range times {
			if _, err := expired.Execute(context.Background(), dto.PaymentExpiredInput{OrderID: "never-a-label"}); err != nil {
				t.Fatal(err)
			}
		}
	}

	runCaptured("applied", nil, 3)
	runCaptured("unexpected", nil, 2)
	runCaptured("noop", nil, 1)
	runCaptured("", errors.New("commit: conn reset"), 4) // a failure: not counted
	runExpired("applied", 2)
	runExpired("ignored", 5)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "ordering.payment_event" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				if dp.Attributes.Len() != 2 {
					t.Errorf("labels = %v, want exactly event and effect", dp.Attributes)
				}
				ev, _ := dp.Attributes.Value("event")
				ef, _ := dp.Attributes.Value("effect")
				got[ev.AsString()+"/"+ef.AsString()] = dp.Value
			}
		}
	}
	want := map[string]int64{"captured/applied": 3, "captured/unexpected": 2, "captured/noop": 1, "expired/applied": 2, "expired/ignored": 5}
	if len(got) != len(want) {
		t.Fatalf("series = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
}
