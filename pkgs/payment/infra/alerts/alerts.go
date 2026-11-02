// Package alerts implements port.Alerts: a fixed log line and a counter, nothing else.
package alerts

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
)

var _ port.Alerts = (*Alerts)(nil)

// Alerts logs an alert at ERROR with its fixed category and the order id, and counts it in
// payment_alerts_total{alert} (the label is the closed set of payment.Alert values: no provider
// text, no customer data, no order id). A Grafana rule on the counter pages an operator.
type Alerts struct {
	log     *slog.Logger
	counter metric.Int64Counter
	// the reconcile backlog (docs/payment.md §3 補查): payments owed a query, and the longest wait since
	// one was last looked at
	eligible metric.Int64Gauge
	oldest   metric.Float64Gauge
}

func New(l *slog.Logger, meter metric.MeterProvider) (*Alerts, error) {
	c, err := meter.Meter("payment").Int64Counter("payment_alerts_total",
		metric.WithDescription("Payment events that need an operator, by fixed category (docs/payment.md)."))
	if err != nil {
		return nil, err
	}

	eligible, err := meter.Meter("payment").Int64Gauge("payment_reconcile_eligible",
		metric.WithDescription("Payments inside their reconcile window that are owed a provider query, after the last sweep."))
	if err != nil {
		return nil, err
	}
	oldest, err := meter.Meter("payment").Float64Gauge("payment_reconcile_oldest_unchecked_seconds",
		metric.WithDescription("How long the least recently looked-at reconcilable payment has waited, after the last sweep."),
		metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}

	return &Alerts{log: l, counter: c, eligible: eligible, oldest: oldest}, nil
}

// Backlog records the reconcile backlog as two gauges, set once per sweep.
func (x *Alerts) Backlog(c context.Context, eligible int, oldest time.Duration) {
	x.eligible.Record(c, int64(eligible))
	x.oldest.Record(c, oldest.Seconds())
}

func (x *Alerts) Raise(c context.Context, a payment.Alert, orderID string) {
	x.log.ErrorContext(c, "payment alert", "alert", string(a), "order_id", orderID)
	x.counter.Add(c, 1, metric.WithAttributes(attribute.String("alert", string(a))))
}
