// Package ordering is the ordering bounded context. ProviderSet is its only
// export to composition roots (apps/*). Use cases are exposed as decorated
// usecasex.UseCase interfaces, so every adapter gets the same cross-cutting behaviour.
package ordering

import (
	"github.com/google/wire"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/consumer"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/rpc"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
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

	// application (decorated)
	ProvideCreateOrder,
	ProvideGetOrder,
	ProvideCancelOrder,

	// adapters
	rpc.NewOrderService,
	consumer.NewOrderCancellationHandler,
)

func ProvideCreateOrder(repo order.Repository, ids port.IDGenerator, clock port.Clock) rpc.CreateOrderUC {
	return usecasex.Chain[dto.CreateOrderInput, *dto.OrderOutput](
		usecase.NewCreateOrder(repo, ids, clock),
		usecasex.Logging[dto.CreateOrderInput, *dto.OrderOutput]("ordering.create_order"),
	)
}

func ProvideGetOrder(repo order.Repository) rpc.GetOrderUC {
	return usecasex.Chain[dto.GetOrderInput, *dto.OrderOutput](
		usecase.NewGetOrder(repo),
		usecasex.Logging[dto.GetOrderInput, *dto.OrderOutput]("ordering.get_order"),
	)
}

func ProvideCancelOrder(repo order.Repository, clock port.Clock) rpc.CancelOrderUC {
	return usecasex.Chain[dto.CancelOrderInput, *dto.OrderOutput](
		usecase.NewCancelOrder(repo, clock),
		usecasex.Logging[dto.CancelOrderInput, *dto.OrderOutput]("ordering.cancel_order"),
	)
}
