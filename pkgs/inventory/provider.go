// Package inventory is the inventory bounded context: stock per SKU, and the
// reservation each order claims. It reacts to ordering's OrderCreated /
// OrderCancelled events and asks ordering to cancel orders it cannot cover.
// ProviderSet is its only export to composition roots (apps/*).
package inventory

import (
	"log/slog"

	"github.com/google/wire"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/adapters/consumer"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/adapters/events"
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

	// application (decorated; see decorate). *usecasex.Recorder and the
	// message.Publisher (outbox) are shared across BCs: apps provide them.
	ProvideSetStock,
	ProvideGetStock,
	ProvideListStocks,
	ProvideGetReservation,
	ProvideReserveStock,
	ProvideReleaseStock,

	// adapters
	rpc.NewInventoryService,
	consumer.NewOrderCreatedHandler,
	consumer.NewOrderCancelledHandler,
	ProvideSubscriptions,
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
func ProvideSubscriptions(created *consumer.OrderCreatedHandler, cancelled *consumer.OrderCancelledHandler) Subscriptions {
	return Subscriptions{
		{Topic: consumer.TopicOrderCreated, Handler: created},
		{Topic: consumer.TopicOrderCancelled, Handler: cancelled},
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
func decorateTx[I, O any](name string, rec *usecasex.Recorder, tx port.Transactor, inbox port.Inbox, uc usecasex.UseCase[I, O]) usecasex.UseCase[I, O] {
	return usecasex.Chain(uc,
		usecasex.Metrics[I, O](rec, name),
		usecasex.Logging[I, O](name),
		usecasex.Transactional[I, O](tx),
		usecasex.Idempotent[I, O](inbox),
	)
}

func ProvideSetStock(stocks stock.Repository, tx port.Transactor, inbox port.Inbox, clock port.Clock, rec *usecasex.Recorder) rpc.SetStockUC {
	return decorateTx("inventory.set_stock", rec, tx, inbox, usecase.NewSetStock(stocks, clock))
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

func ProvideReserveStock(reservations reservation.Repository, stocks stock.Repository, tx port.Transactor, inbox port.Inbox, events port.EventPublisher, clock port.Clock, rec *usecasex.Recorder) consumer.ReserveStockUC {
	return decorateTx("inventory.reserve_stock", rec, tx, inbox, usecase.NewReserveStock(reservations, stocks, events, clock))
}

func ProvideReleaseStock(reservations reservation.Repository, stocks stock.Repository, tx port.Transactor, inbox port.Inbox, events port.EventPublisher, clock port.Clock, rec *usecasex.Recorder) consumer.ReleaseStockUC {
	return decorateTx("inventory.release_stock", rec, tx, inbox, usecase.NewReleaseStock(reservations, stocks, events, clock))
}
