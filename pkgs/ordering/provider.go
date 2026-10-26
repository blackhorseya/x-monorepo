// Package ordering is the ordering bounded context. ProviderSet is its only
// export to composition roots (apps/*). Use cases are exposed as decorated
// usecasex.UseCase interfaces, so every adapter gets the same cross-cutting behaviour.
package ordering

import (
	"log/slog"

	"github.com/google/wire"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/consumer"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/events"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/metrics"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/rpc"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/system"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// Schema is the Postgres schema of this BC (tables, migrations, event topics).
const Schema = postgres.Schema

var ProviderSet = wire.NewSet(
	// infra → ports / repositories
	ProvideOrderRepository,
	ProvideTransactor,
	ProvideInbox,
	events.NewPublisher,
	wire.Bind(new(port.EventPublisher), new(*events.Publisher)),
	system.NewUUIDGenerator,
	wire.Bind(new(port.IDGenerator), new(*system.UUIDGenerator)),
	system.NewClock,
	wire.Bind(new(port.Clock), new(*system.Clock)),

	// application (decorated; see decorate). *usecasex.Recorder and the
	// message.Publisher (outbox) are shared across BCs: apps provide them.
	ProvideCreateOrder,
	ProvideGetOrder,
	ProvideCancelOrder,
	ProvideConfirmOrder,
	ProvideListOrders,
	ProvideOrderStats,

	// adapters
	rpc.NewOrderService,
	consumer.NewOrderCancellationHandler,
	consumer.NewStockReservedHandler,
	ProvideSubscriptions,
	ProvideGauges,
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

// ProvideInbox is this BC's consumer inbox (usecasex.Idempotent): the
// `<schema>.inbox` table on Postgres, none in memory — a nil Inbox makes
// Idempotent a pass-through, which is right because the in-memory transactor
// could not roll a record back with a failed attempt, and the in-memory bus
// is at most once anyway.
func ProvideInbox(db *gorm.DB) port.Inbox {
	if db == nil {
		return nil
	}

	return platformpg.NewInbox(db, Schema)
}

// Subscriptions lists what this BC consumes; apps/worker registers them on
// the router with this BC's poison topic. A named type, so wire can tell each
// BC's subscriptions apart.
type Subscriptions []eventbus.Subscription

// ProvideSubscriptions binds each consumed topic to its handler. The handler
// name is the topic.
func ProvideSubscriptions(cancel *consumer.OrderCancellationHandler, reserved *consumer.StockReservedHandler) Subscriptions {
	return Subscriptions{
		{Topic: consumer.Topic, Handler: cancel},
		{Topic: consumer.TopicStockReserved, Handler: reserved},
	}
}

// Gauges is this BC's funnel gauges (adapters/metrics: orders per status and
// the age of the oldest in each); apps/worker registers them next to the
// eventbus lag gauges. A named type so wire can tell each BC's apart.
type Gauges struct{ *metrics.Gauges }

func ProvideGauges(stats metrics.OrderStatsUC) Gauges { return Gauges{metrics.NewGauges(stats)} }

// decorate is the one place that decides which cross-cutting decorators every
// use case of this BC gets (outermost first). Add a decorator here, not per use case.
// Reads use it as is: a transaction would only add a BEGIN/COMMIT round trip.
func decorate[I, O any](name string, rec *usecasex.Recorder, uc usecasex.UseCase[I, O]) usecasex.UseCase[I, O] {
	return usecasex.Chain(uc,
		usecasex.Metrics[I, O](rec, name),
		usecasex.Logging[I, O](name),
	)
}

// decorateTx is decorate for use cases that write: the use case runs inside
// one transaction, so its repository writes and outbox events commit or roll
// back together. The transaction is innermost but one, so a failed commit is
// logged and counted as the use case's failure; inside it, Idempotent records
// the consumed message id in this BC's inbox (same transaction: it rolls back
// with a failed attempt) and drops a redelivery before any aggregate is
// loaded. On the rpc path there is no message id and it is a pass-through.
func decorateTx[I, O any](name string, rec *usecasex.Recorder, tx port.Transactor, inbox port.Inbox, uc usecasex.UseCase[I, O]) usecasex.UseCase[I, O] {
	return usecasex.Chain(uc,
		usecasex.Metrics[I, O](rec, name),
		usecasex.Logging[I, O](name),
		usecasex.Transactional[I, O](tx),
		usecasex.Idempotent[I, O](inbox),
	)
}

func ProvideCreateOrder(repo order.Repository, tx port.Transactor, inbox port.Inbox, events port.EventPublisher, ids port.IDGenerator, clock port.Clock, rec *usecasex.Recorder) rpc.CreateOrderUC {
	return decorateTx("ordering.create_order", rec, tx, inbox, usecase.NewCreateOrder(repo, events, ids, clock))
}

func ProvideGetOrder(repo order.Repository, rec *usecasex.Recorder) rpc.GetOrderUC {
	return decorate("ordering.get_order", rec, usecase.NewGetOrder(repo))
}

func ProvideCancelOrder(repo order.Repository, tx port.Transactor, inbox port.Inbox, events port.EventPublisher, clock port.Clock, rec *usecasex.Recorder) rpc.CancelOrderUC {
	return decorateTx("ordering.cancel_order", rec, tx, inbox, usecase.NewCancelOrder(repo, events, clock))
}

// ProvideConfirmOrder has no rpc caller: only the consumer (inventory.v1.StockReserved) drives it.
func ProvideConfirmOrder(repo order.Repository, tx port.Transactor, inbox port.Inbox, events port.EventPublisher, clock port.Clock, rec *usecasex.Recorder) consumer.ConfirmOrderUC {
	return decorateTx("ordering.confirm_order", rec, tx, inbox, usecase.NewConfirmOrder(repo, events, clock))
}

// ProvideOrderStats feeds the gauges; decorated like every read (its Logging
// success line is Debug, so a scrape every 15s stays quiet at Info).
func ProvideOrderStats(repo order.Repository, rec *usecasex.Recorder) metrics.OrderStatsUC {
	return decorate("ordering.order_stats", rec, usecase.NewOrderStats(repo))
}

func ProvideListOrders(repo order.Repository, rec *usecasex.Recorder) rpc.ListOrdersUC {
	return decorate("ordering.list_orders", rec, usecase.NewListOrders(repo))
}
