package metrics

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// What CreateOrder ended in, the only label of ordering_create_order_total:
//
//	created     a new order was written (with or without an idempotency key)
//	replayed    the key had already created this very request: the original order came back
//	key_reused  the key had already been spent on a different request
//
// Every other failure (validation, permission, an infrastructure error, a failed
// commit) is deliberately not counted here: usecase_duration_seconds{usecase=
// "ordering.create_order", kind} already splits them by kernel error kind, and a
// catch-all "rejected" would only mix causes with different owners. The label is
// the result and nothing else: never the idempotency key, the customer or an
// order id (R4, and cardinality).
const (
	ResultCreated   = "created"
	ResultReplayed  = "replayed"
	ResultKeyReused = "key_reused"
)

// CreateOrderResults counts CreateOrder outcomes (Prometheus:
// ordering_create_order_total{result}). A counter: sum across replicas.
type CreateOrderResults struct {
	results metric.Int64Counter
}

// NewCreateOrderResults creates ordering.create_order on mp. rpc registers it
// (it is the process that runs CreateOrder); the worker does not.
func NewCreateOrderResults(mp metric.MeterProvider) (*CreateOrderResults, error) {
	c, err := mp.Meter("github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/metrics").Int64Counter(
		"ordering.create_order",
		metric.WithUnit("{order}"),
		metric.WithDescription("CreateOrder outcomes by result: created | replayed (same idempotency key, same items) | key_reused (same key, different items). Other failures are in usecase.duration by kind."),
	)
	if err != nil {
		return nil, fmt.Errorf("create ordering.create_order: %w", err)
	}

	return &CreateOrderResults{results: c}, nil
}

// Decorator counts after next returns, so it belongs outside the transaction:
// a use case whose commit failed returns an error and is never counted as
// created, and a race loser that read the winner back reports Replayed.
func (h *CreateOrderResults) Decorator() usecasex.Decorator[dto.CreateOrderInput, *dto.OrderOutput] {
	return func(next usecasex.UseCase[dto.CreateOrderInput, *dto.OrderOutput]) usecasex.UseCase[dto.CreateOrderInput, *dto.OrderOutput] {
		return usecasex.Func[dto.CreateOrderInput, *dto.OrderOutput](func(c context.Context, in dto.CreateOrderInput) (*dto.OrderOutput, error) {
			out, err := next.Execute(c, in)

			switch {
			case err == nil && out.Replayed:
				h.count(c, ResultReplayed)
			case err == nil:
				h.count(c, ResultCreated)
			case errors.Is(err, order.ErrIdempotencyKeyReused):
				h.count(c, ResultKeyReused)
			}

			return out, err
		})
	}
}

func (h *CreateOrderResults) count(c context.Context, result string) {
	h.results.Add(c, 1, metric.WithAttributes(attribute.String("result", result)))
}
