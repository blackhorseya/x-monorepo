// Package ordering is the ordering bounded context. ProviderSet is its only
// export to composition roots (apps/*). Use cases are exposed as decorated
// usecasex.UseCase interfaces, so every adapter gets the same cross-cutting behaviour.
package ordering

import (
	"github.com/google/wire"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/consumer"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/rpc"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/system"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

var ProviderSet = wire.NewSet(
	// infra → ports / repositories
	memory.NewOrderRepository,
	wire.Bind(new(order.Repository), new(*memory.OrderRepository)),
	system.NewUUIDGenerator,
	wire.Bind(new(port.IDGenerator), new(*system.UUIDGenerator)),
	system.NewClock,
	wire.Bind(new(port.Clock), new(*system.Clock)),

	// application (decorated; see decorate)
	usecasex.NewRecorder,
	ProvideCreateOrder,
	ProvideGetOrder,
	ProvideCancelOrder,

	// adapters
	rpc.NewOrderService,
	consumer.NewOrderCancellationHandler,
)

// decorate is the one place that decides which cross-cutting decorators every
// use case of this BC gets (outermost first). Add a decorator here, not per use case.
func decorate[I, O any](name string, rec *usecasex.Recorder, uc usecasex.UseCase[I, O]) usecasex.UseCase[I, O] {
	return usecasex.Chain(uc,
		usecasex.Metrics[I, O](rec, name),
		usecasex.Logging[I, O](name),
	)
}

func ProvideCreateOrder(repo order.Repository, ids port.IDGenerator, clock port.Clock, rec *usecasex.Recorder) rpc.CreateOrderUC {
	return decorate("ordering.create_order", rec, usecase.NewCreateOrder(repo, ids, clock))
}

func ProvideGetOrder(repo order.Repository, rec *usecasex.Recorder) rpc.GetOrderUC {
	return decorate("ordering.get_order", rec, usecase.NewGetOrder(repo))
}

func ProvideCancelOrder(repo order.Repository, clock port.Clock, rec *usecasex.Recorder) rpc.CancelOrderUC {
	return decorate("ordering.cancel_order", rec, usecase.NewCancelOrder(repo, clock))
}
