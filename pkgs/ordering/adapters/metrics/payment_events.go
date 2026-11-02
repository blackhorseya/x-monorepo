package metrics

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// PaymentEventResults counts what a payment event did to its order (Prometheus:
// ordering_payment_event_total{event,effect}). event is "captured" or "expired"; effect is
// order.PaymentEffect (applied | ignored | noop | unexpected). "unexpected" is the one to alert
// on: a PaymentCaptured for a CONFIRMED order whose snapshot says no payment cannot happen unless
// something upstream is wrong (docs/payment.md §8). Refused events are not counted here: they are
// domain errors, in usecase.duration by kind. Never an order id or a customer as a label.
type PaymentEventResults struct {
	results metric.Int64Counter
}

// NewPaymentEventResults creates ordering.payment_event on mp. The worker registers it (it is the
// process that consumes payment's events).
func NewPaymentEventResults(mp metric.MeterProvider) (*PaymentEventResults, error) {
	c, err := mp.Meter("github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/metrics").Int64Counter(
		"ordering.payment_event",
		metric.WithUnit("{event}"),
		metric.WithDescription("Payment events applied to orders by event (captured | expired) and effect (applied | ignored | noop | unexpected)."),
	)
	if err != nil {
		return nil, fmt.Errorf("create ordering.payment_event: %w", err)
	}

	return &PaymentEventResults{results: c}, nil
}

// Captured counts after next returns, outside the transaction: a failed commit is never counted.
func (h *PaymentEventResults) Captured() usecasex.Decorator[dto.PaymentCapturedInput, *dto.PaymentEventOutput] {
	return decorate[dto.PaymentCapturedInput](h, "captured")
}

// Expired is Captured for payment.v1.PaymentExpired.
func (h *PaymentEventResults) Expired() usecasex.Decorator[dto.PaymentExpiredInput, *dto.PaymentEventOutput] {
	return decorate[dto.PaymentExpiredInput](h, "expired")
}

func decorate[I any](h *PaymentEventResults, event string) usecasex.Decorator[I, *dto.PaymentEventOutput] {
	return func(next usecasex.UseCase[I, *dto.PaymentEventOutput]) usecasex.UseCase[I, *dto.PaymentEventOutput] {
		return usecasex.Func[I, *dto.PaymentEventOutput](func(c context.Context, in I) (*dto.PaymentEventOutput, error) {
			out, err := next.Execute(c, in)
			if err == nil {
				h.results.Add(c, 1, metric.WithAttributes(attribute.String("event", event), attribute.String("effect", out.Effect)))
			}

			return out, err
		})
	}
}
