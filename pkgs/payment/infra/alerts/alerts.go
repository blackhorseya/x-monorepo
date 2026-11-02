// Package alerts implements port.Alerts: a fixed log line and a counter, nothing else.
package alerts

import (
	"context"
	"log/slog"

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
}

func New(l *slog.Logger, meter metric.MeterProvider) (*Alerts, error) {
	c, err := meter.Meter("payment").Int64Counter("payment_alerts_total",
		metric.WithDescription("Payment events that need an operator, by fixed category (docs/payment.md)."))
	if err != nil {
		return nil, err
	}

	return &Alerts{log: l, counter: c}, nil
}

func (x *Alerts) Raise(c context.Context, a payment.Alert, orderID string) {
	x.log.ErrorContext(c, "payment alert", "alert", string(a), "order_id", orderID)
	x.counter.Add(c, 1, metric.WithAttributes(attribute.String("alert", string(a))))
}
