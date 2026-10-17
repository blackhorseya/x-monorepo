// Package ordering is the ordering bounded context. ProviderSet is its only
// export to composition roots (apps/*). Use cases are exposed as decorated
// usecasex.UseCase interfaces, so every adapter gets the same cross-cutting behaviour.
package ordering

import (
	"log/slog"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/google/wire"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/consumer"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/events"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/rpc"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/system"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// Schema is the Postgres schema of this BC (tables, migrations, event topics).
const Schema = postgres.Schema

var ProviderSet = wire.NewSet(
	// infra → ports / repositories
	ProvideOrderRepository,
	ProvideTransactor,
	ProvidePublisher,
	events.NewPublisher,
	wire.Bind(new(port.EventPublisher), new(*events.Publisher)),
	system.NewUUIDGenerator,
	wire.Bind(new(port.IDGenerator), new(*system.UUIDGenerator)),
	system.NewClock,
	wire.Bind(new(port.Clock), new(*system.Clock)),

	// application (decorated; see decorate)
	usecasex.NewRecorder,
	ProvideCreateOrder,
	ProvideGetOrder,
	ProvideCancelOrder,
	ProvideListOrders,

	// adapters
	rpc.NewOrderService,
	consumer.NewOrderCancellationHandler,
	Subscriptions,
)

// ProvideOrderRepository picks the persistence backend from the composition
// root's database handle: nil (APP_POSTGRES_URL unset) means in-memory — local dev
// and tests — otherwise Postgres. apps never run migrations; see apps/migrate.
func ProvideOrderRepository(db *gorm.DB, logger *slog.Logger) order.Repository {
	if db == nil {
		logger.Warn("ordering: APP_POSTGRES_URL unset, using in-memory order repository")
		return memory.NewOrderRepository()
	}

	return postgres.NewOrderRepository(db)
}

// ProvideTransactor pairs with ProvideOrderRepository: a real transaction on
// Postgres, a pass-through in memory.
func ProvideTransactor(db *gorm.DB) port.Transactor {
	if db == nil {
		return memory.NewTransactor()
	}

	return postgres.NewTransactor(db)
}

// ProvidePublisher is the outbox: watermill-sql rows in this BC's schema,
// written inside the use case's transaction, or GoChannel (in-process, at
// most once) when there is no database. Both carry trace context.
func ProvidePublisher(db *gorm.DB, logger *slog.Logger, tel *otel.Providers) (message.Publisher, func(), error) {
	if db == nil {
		ch := eventbus.NewGoChannel(logger)
		return eventbus.Traced(ch, tel), func() { _ = ch.Close() }, nil
	}

	pub, err := eventbus.NewPostgresPublisher(db, logger)
	if err != nil {
		return nil, nil, err
	}

	return eventbus.Traced(pub, tel), func() {}, nil
}

// Subscriptions lists what this BC consumes; apps/worker registers them on
// the router. The handler name is the topic.
func Subscriptions(cancel *consumer.OrderCancellationHandler) []eventbus.Subscription {
	return []eventbus.Subscription{{Topic: consumer.Topic, Handler: cancel}}
}

// decorate is the one place that decides which cross-cutting decorators every
// use case of this BC gets (outermost first). Add a decorator here, not per use case.
func decorate[I, O any](name string, rec *usecasex.Recorder, uc usecasex.UseCase[I, O]) usecasex.UseCase[I, O] {
	return usecasex.Chain(uc,
		usecasex.Metrics[I, O](rec, name),
		usecasex.Logging[I, O](name),
	)
}

func ProvideCreateOrder(repo order.Repository, tx port.Transactor, events port.EventPublisher, ids port.IDGenerator, clock port.Clock, rec *usecasex.Recorder) rpc.CreateOrderUC {
	return decorate("ordering.create_order", rec, usecase.NewCreateOrder(repo, tx, events, ids, clock))
}

func ProvideGetOrder(repo order.Repository, rec *usecasex.Recorder) rpc.GetOrderUC {
	return decorate("ordering.get_order", rec, usecase.NewGetOrder(repo))
}

func ProvideCancelOrder(repo order.Repository, tx port.Transactor, events port.EventPublisher, clock port.Clock, rec *usecasex.Recorder) rpc.CancelOrderUC {
	return decorate("ordering.cancel_order", rec, usecase.NewCancelOrder(repo, tx, events, clock))
}

func ProvideListOrders(repo order.Repository, rec *usecasex.Recorder) rpc.ListOrdersUC {
	return decorate("ordering.list_orders", rec, usecase.NewListOrders(repo))
}
