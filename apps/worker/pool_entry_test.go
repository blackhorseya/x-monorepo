package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory"
	inventoryconsumer "github.com/blackhorseya/x-monorepo/pkgs/inventory/adapters/consumer"
	inventoryevents "github.com/blackhorseya/x-monorepo/pkgs/inventory/adapters/events"
	inventorypg "github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	inventorysys "github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/system"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering"
	orderingconsumer "github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/consumer"
	orderingevents "github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/events"
	orderingpg "github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	orderingsys "github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/system"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
	platformredis "github.com/blackhorseya/x-monorepo/pkgs/platform/redis"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/redis/redistest"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// Integration (pgtest, redistest): the pool floor is enforced where the worker
// really starts. NewWorker is entered with the real BC subscriptions (2 ordering
// + 3 inventory = 5 subscribers, floor 2 x 5 + 1 = 11) and a pool capped by
// postgres.max_open_conns, for both brokers. A direct call of checkPool would
// stay green if NewWorker stopped calling it.
func TestNewWorker_RefusesPoolBelowFloor(t *testing.T) {
	for _, broker := range []string{config.BrokerPostgres, config.BrokerRedis} {
		t.Run(broker, func(t *testing.T) {
			var rdb goredis.UniversalClient
			if broker == config.BrokerRedis {
				client, cleanup, err := platformredis.NewClient(config.Redis{URL: redistest.URL(t)})
				if err != nil {
					t.Fatal(err)
				}
				defer cleanup()
				rdb = client
			}
			for _, tc := range []struct {
				name    string
				maxOpen int
				wantErr bool
			}{
				{"below the floor", 10, true},
				{"at the floor", 11, false},
				{"unlimited", 0, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					_, err := newWorkerWithPool(t, broker, rdb, tc.maxOpen)
					switch {
					case tc.wantErr && err == nil:
						t.Fatalf("NewWorker accepted max_open_conns=%d, want a refusal", tc.maxOpen)
					case tc.wantErr && !strings.Contains(err.Error(), "postgres.max_open_conns 10 is below the 11"):
						t.Fatalf("error does not say why: %v", err)
					case !tc.wantErr && err != nil:
						t.Fatalf("NewWorker(max_open_conns=%d): %v", tc.maxOpen, err)
					}
				})
			}
		})
	}
}

// newWorkerWithPool opens a pool capped at maxOpen and calls NewWorker with the
// real subscriptions, exactly as apps/worker wires them.
func newWorkerWithPool(t *testing.T, broker string, rdb goredis.UniversalClient, maxOpen int) (*Worker, error) {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pgCfg := config.Postgres{URL: pgtest.URL(t), MaxOpenConns: maxOpen}
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(telCleanup)
	db, dbCleanup, err := postgres.NewDB(pgCfg, tel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dbCleanup)
	for _, m := range []postgres.Migration{orderingpg.Migration(), inventorypg.Migration()} {
		if err := postgres.Up(c, pgCfg, m); err != nil {
			t.Fatal(err)
		}
	}
	logger := tel.Logger
	publisher, pubCleanup, err := eventbus.ProvidePublisher(db, logger, tel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pubCleanup)
	rec, err := usecasex.NewRecorder(tel.Meter)
	if err != nil {
		t.Fatal(err)
	}

	orders := ordering.ProvideOrderRepository(db, logger)
	otx, oinbox := ordering.ProvideTransactor(db), ordering.ProvideInbox(db)
	oev, oclock := orderingevents.NewPublisher(publisher), orderingsys.NewClock()
	orderingSubs := ordering.ProvideSubscriptions(
		orderingconsumer.NewOrderCancellationHandler(ordering.ProvideCancelOrder(orders, otx, oinbox, oev, oclock, rec)),
		orderingconsumer.NewStockReservedHandler(ordering.ProvideConfirmOrder(orders, otx, oinbox, oev, oclock, rec)))
	stocks, reservations := inventory.ProvideStockRepository(db, logger), inventory.ProvideReservationRepository(db)
	itx, iinbox := inventory.ProvideTransactor(db), inventory.ProvideInbox(db)
	iev, iclock := inventoryevents.NewPublisher(publisher), inventorysys.NewClock()
	inventorySubs := inventory.ProvideSubscriptions(
		inventoryconsumer.NewOrderCreatedHandler(inventory.ProvideReserveStock(reservations, stocks, itx, iinbox, iev, iclock, rec)),
		inventoryconsumer.NewOrderCancelledHandler(inventory.ProvideReleaseStock(reservations, stocks, itx, iinbox, iev, iclock, rec)),
		inventoryconsumer.NewOrderCompletedHandler(inventory.ProvideCommitStock(reservations, stocks, itx, iinbox, iev, iclock, rec)),
	)

	group := fmt.Sprintf("pool-%d", time.Now().UnixNano())
	return NewWorker(config.Worker{ConsumerGroup: group, Broker: broker}, db, rdb, logger, tel, publisher, orderingSubs, inventorySubs,
		ordering.ProvideGauges(ordering.ProvideOrderStats(orders, rec)), inventory.ProvideGauges(inventory.ProvideReservationStats(reservations, rec)))
}
