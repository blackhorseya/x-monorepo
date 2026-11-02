package alerts_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/alerts"
)

// An alert is a fixed-category ERROR line with the order id and one count of payment_alerts_total
// labelled with the category only.
func TestRaise(t *testing.T) {
	logs := &bytes.Buffer{}
	reader := sdkmetric.NewManualReader()
	a, err := alerts.New(slog.New(slog.NewTextHandler(logs, nil)), sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	if err != nil {
		t.Fatal(err)
	}
	a.Raise(context.Background(), payment.AlertAmountMismatch, "ord-7")
	a.Raise(context.Background(), payment.AlertAmountMismatch, "ord-8")

	if out := logs.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "alert=amount_mismatch") || !strings.Contains(out, "order_id=ord-7") {
		t.Fatalf("log = %q", out)
	}
	var rm metricdata.ResourceMetrics
	if err = reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "payment_alerts_total" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				if v, _ := dp.Attributes.Value("alert"); v.AsString() != "amount_mismatch" || dp.Attributes.Len() != 1 {
					t.Fatalf("attributes = %v, want only alert=amount_mismatch", dp.Attributes)
				}
				total += dp.Value
			}
		}
	}
	if total != 2 {
		t.Fatalf("payment_alerts_total = %d, want 2 (a count with no data would pass an absence check)", total)
	}
}
