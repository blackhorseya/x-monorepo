// Package inventory is the inventory bounded context: stock per SKU, and the
// reservation each order claims. It reacts to ordering's OrderCreated /
// OrderCancelled / OrderCompleted events and asks ordering to cancel orders it cannot cover
// — or held past the reservation TTL (ExpireReservations: `worker expire`, the rpc, the CronJob).
// ProviderSet is its only export to composition roots (apps/*).
package inventory

import (
	"log/slog"

	"github.com/google/wire"
	"gorm.io/gorm"

	catalogadapter "github.com/blackhorseya/x-monorepo/pkgs/inventory/adapters/catalog"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/adapters/cli"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/adapters/consumer"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/adapters/events"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/adapters/metrics"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/adapters/rpc"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/stock"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/system"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// Schema is the Postgres schema of this BC (tables, migrations, event topics).
const Schema = postgres.Schema

var ProviderSet = wire.NewSet(
	// infra → ports / repositories
	ProvideStockRepository,
	ProvideReservationRepository,
	ProvideTransactor,
	ProvideInbox,
	events.NewPublisher,
	wire.Bind(new(port.EventPublisher), new(*events.Publisher)),
	system.NewClock,
	wire.Bind(new(port.Clock), new(*system.Clock)),
	// SetStock asks the catalog, in process, whether a SKU is a product. Only apps/rpc binds
	// the catalog's handler; apps/worker never builds SetStock and so never needs it.
	catalogadapter.NewCatalog,
	wire.Bind(new(port.ProductCatalog), new(*catalogadapter.Catalog)),

	// application (decorated; see decorate). *usecasex.Recorder and the
	// message.Publisher (outbox) are shared across BCs: apps provide them.
	ProvideSetStock,
	ProvideGetStock,
	ProvideListStocks,
	ProvideGetReservation,
	ProvideBatchGetReservations,
	ProvideListReservations,
	ProvideReservationStats,
	ProvideReserveStock,
	ProvideReleaseStock,
	ProvideCommitStock,
	ProvideExpireReservations,

	// adapters
	rpc.NewInventoryService,
	cli.NewExpire,
	consumer.NewOrderCreatedHandler,
	consumer.NewOrderCancelledHandler,
	consumer.NewOrderCompletedHandler,
	ProvideSubscriptions,
	ProvideGauges,
)

// ProvideStockRepository picks the persistence backend from the composition
// root's database handle: nil (APP_POSTGRES_URL unset) means in-memory.
func ProvideStockRepository(db *gorm.DB, logger *slog.Logger) stock.Repository {
	if db == nil {
		logger.Warn("inventory: APP_POSTGRES_URL unset, using in-memory stock repository")
		return memory.NewStockRepository()
	}

	return postgres.NewStockRepository(db)
}

func ProvideReservationRepository(db *gorm.DB) reservation.Repository {
	if db == nil {
		return memory.NewReservationRepository()
	}

	return postgres.NewReservationRepository(db)
}

// ProvideTransactor pairs with the repositories: a real transaction on
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
func ProvideSubscriptions(created *consumer.OrderCreatedHandler, cancelled *consumer.OrderCancelledHandler, completed *consumer.OrderCompletedHandler) Subscriptions {
	return Subscriptions{
		{Topic: consumer.TopicOrderCreated, Handler: created},
		{Topic: consumer.TopicOrderCancelled, Handler: cancelled},
		{Topic: consumer.TopicOrderCompleted, Handler: completed},
	}
}

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
//
// guards run after Logging and before the transaction opens: they may refuse without ever taking
// a connection, and Metrics and Logging still see what they refuse.
func decorateTx[I, O any](name string, rec *usecasex.Recorder, tx port.Transactor, inbox port.Inbox, uc usecasex.UseCase[I, O], guards ...usecasex.Decorator[I, O]) usecasex.UseCase[I, O] {
	return usecasex.Chain(uc, append(append([]usecasex.Decorator[I, O]{
		usecasex.Metrics[I, O](rec, name),
		usecasex.Logging[I, O](name),
	}, guards...),
		usecasex.Transactional[I, O](tx),
		usecasex.Idempotent[I, O](inbox),
	)...)
}

// ProvideSetStock stocks only what the catalog has: requireProduct asks it before the
// transaction opens.
func ProvideSetStock(stocks stock.Repository, tx port.Transactor, inbox port.Inbox, clock port.Clock, products port.ProductCatalog, rec *usecasex.Recorder) rpc.SetStockUC {
	return decorateTx("inventory.set_stock", rec, tx, inbox, usecase.NewSetStock(stocks, clock), requireProduct(products))
}

func ProvideGetStock(stocks stock.Repository, rec *usecasex.Recorder) rpc.GetStockUC {
	return decorate("inventory.get_stock", rec, usecase.NewGetStock(stocks))
}

func ProvideListStocks(stocks stock.Repository, rec *usecasex.Recorder) rpc.ListStocksUC {
	return decorate("inventory.list_stocks", rec, usecase.NewListStocks(stocks))
}

func ProvideGetReservation(reservations reservation.Repository, rec *usecasex.Recorder) rpc.GetReservationUC {
	return decorate("inventory.get_reservation", rec, usecase.NewGetReservation(reservations))
}

// Gauges is this BC's funnel gauges (adapters/metrics: reservations per status
// and the age of the oldest in each); apps/worker registers them next to the
// eventbus lag gauges. A named type so wire can tell each BC's apart.
type Gauges struct{ *metrics.Gauges }

func ProvideGauges(stats metrics.ReservationStatsUC) Gauges { return Gauges{metrics.NewGauges(stats)} }

// ProvideReservationStats feeds the gauges; decorated like every read (Logging's
// success line is Debug, so a scrape every 15s stays quiet at Info).
func ProvideReservationStats(reservations reservation.Repository, rec *usecasex.Recorder) metrics.ReservationStatsUC {
	return decorate("inventory.reservation_stats", rec, usecase.NewReservationStats(reservations))
}

func ProvideBatchGetReservations(reservations reservation.Repository, rec *usecasex.Recorder) rpc.BatchGetReservationsUC {
	return decorate("inventory.batch_get_reservations", rec, usecase.NewBatchGetReservations(reservations))
}

func ProvideListReservations(reservations reservation.Repository, rec *usecasex.Recorder) rpc.ListReservationsUC {
	return decorate("inventory.list_reservations", rec, usecase.NewListReservations(reservations))
}

func ProvideReserveStock(reservations reservation.Repository, stocks stock.Repository, tx port.Transactor, inbox port.Inbox, events port.EventPublisher, clock port.Clock, rec *usecasex.Recorder) consumer.ReserveStockUC {
	return decorateTx("inventory.reserve_stock", rec, tx, inbox, usecase.NewReserveStock(reservations, stocks, events, clock))
}

func ProvideReleaseStock(reservations reservation.Repository, stocks stock.Repository, tx port.Transactor, inbox port.Inbox, events port.EventPublisher, clock port.Clock, rec *usecasex.Recorder) consumer.ReleaseStockUC {
	return decorateTx("inventory.release_stock", rec, tx, inbox, usecase.NewReleaseStock(reservations, stocks, events, clock))
}

func ProvideCommitStock(reservations reservation.Repository, stocks stock.Repository, tx port.Transactor, inbox port.Inbox, events port.EventPublisher, clock port.Clock, rec *usecasex.Recorder) consumer.CommitStockUC {
	return decorateTx("inventory.commit_stock", rec, tx, inbox, usecase.NewCommitStock(reservations, stocks, events, clock))
}

// ProvideExpireReservations is the reservation TTL sweep, shared by the rpc
// adapter (operator) and the cli adapter (`worker expire`, the CronJob); the
// type is the plain use case, each adapter names its own alias of it.
func ProvideExpireReservations(reservations reservation.Repository, stocks stock.Repository, tx port.Transactor, inbox port.Inbox, events port.EventPublisher, clock port.Clock, rec *usecasex.Recorder) rpc.ExpireReservationsUC {
	return decorateTx("inventory.expire_reservations", rec, tx, inbox, usecase.NewExpireReservations(reservations, stocks, events, clock))
}
