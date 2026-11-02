package main

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"google.golang.org/protobuf/proto"

	inventoryv1 "github.com/blackhorseya/x-monorepo/pkgs/inventory/contract/v1"
	inventorypg "github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering"
	orderingconsumer "github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/consumer"
	orderingevents "github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/events"
	orderingdto "github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	orderingpg "github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	orderingsys "github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/system"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

const requirePaymentEnv = "APP_ORDERING_REQUIRE_PAYMENT"

// Real entry (docs/payment.md §2, §8): the order's payment_required snapshot is taken in
// the worker, because CONFIRMED happens there (the StockReserved consumer). The worker is
// started the way main starts it — newRootCmd(config.Load) with the setting in the
// environment — so a setting wired only into apps/rpc, or a config key that never reaches
// the consumer, fails here. The check is CompleteOrder on the confirmed order, built from
// the same ordering.ProviderSet apps/rpc serves: it must answer order.payment_required.
//
// Cells: switch true → CONFIRMED snapshot true, CompleteOrder refused; switch flipped back
// to the default (false) → the SAME order is still refused; default switch → a new order
// confirmed under it completes. The default cell sets nothing: it tests the default value.
func TestWorker_RequirePaymentSnapshot_RealEntry(t *testing.T) {
	if v, ok := os.LookupEnv(requirePaymentEnv); ok {
		t.Fatalf("%s=%q is set in the environment: the default cell must run without it", requirePaymentEnv, v)
	}

	url := pgtest.URL(t)
	c, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pgCfg := config.Postgres{URL: url}
	tel := pgtest.Telemetry(t)
	db := pgtest.DB(t, pgCfg, tel)
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
	completeOrder := ordering.ProvideCompleteOrder(orders, ordering.ProvideTransactor(db), ordering.ProvideInbox(db),
		orderingevents.NewPublisher(publisher), orderingsys.NewClock(), ordering.ProvideOwnership(config.Auth{}), rec)

	// Unique ids per run: -count=N and other tests' rows never collide, and the cleanup
	// below deletes exactly what this test created.
	runID := strconv.FormatInt(time.Now().UnixNano(), 36)
	var seeded []string
	t.Cleanup(func() { // before the pool closes (LIFO), errors reported
		bg := context.Background()
		for _, id := range seeded {
			if err := db.WithContext(bg).Exec(`DELETE FROM ordering.orders WHERE id = ?`, id).Error; err != nil {
				t.Errorf("cleanup order %s: %v", id, err)
			}
		}
		// The StockReserved rows this test published, and the consumer-group cursors its workers left
		// on every topic (a stale cursor holds rows back from prune and trips the lag alerts).
		if err := db.WithContext(bg).Exec(`DELETE FROM inventory.events_v1_stockreserved WHERE metadata->>'key' LIKE ?`, "snap-%-"+runID).Error; err != nil {
			t.Errorf("cleanup StockReserved rows: %v", err)
		}
		var tables []string
		if err := db.WithContext(bg).Raw(`SELECT table_schema || '.' || table_name FROM information_schema.tables WHERE table_schema IN ('ordering', 'inventory') AND table_name LIKE '%\_offsets'`).Scan(&tables).Error; err != nil {
			t.Errorf("cleanup: list offsets tables: %v", err)
		}
		for _, tbl := range tables {
			if err := db.WithContext(bg).Exec(`DELETE FROM `+tbl+` WHERE consumer_group LIKE ?`, "snap-"+runID+"-%").Error; err != nil {
				t.Errorf("cleanup cursors in %s: %v", tbl, err)
			}
		}
	})

	// seed saves a PENDING order and answers StockReserved for it, as inventory would.
	seed := func(t *testing.T, name string) string {
		t.Helper()
		usd, _ := kernel.NewMoney(250, "USD")
		li, err := order.NewLineItem("SKU-1", 2, usd)
		if err != nil {
			t.Fatal(err)
		}
		id := "snap-" + name + "-" + runID
		o, err := order.New(order.Params{ID: order.ID(id), CustomerID: "c-snap", Items: []order.LineItem{li}, Now: time.Now().UTC()})
		if err != nil {
			t.Fatal(err)
		}
		if err := orders.Save(c, o); err != nil {
			t.Fatal(err)
		}
		seeded = append(seeded, id)

		return id
	}
	reserve := func(t *testing.T, id string) {
		t.Helper()
		payload, err := proto.Marshal(&inventoryv1.StockReserved{OrderId: id})
		if err != nil {
			t.Fatal(err)
		}
		msg := message.NewMessageWithContext(c, watermill.NewUUID(), payload)
		msg.Metadata.Set(eventbus.MetaKey, id)
		if err := publisher.Publish(orderingconsumer.TopicStockReserved, msg); err != nil {
			t.Fatal(err)
		}
	}
	waitConfirmed := func(t *testing.T, id string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			o, err := orders.FindByID(c, order.ID(id))
			if err != nil {
				t.Fatal(err)
			}
			if o.Status() == order.StatusConfirmed {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("order %s is %s after 30s: the worker never consumed StockReserved", id, o.Status())
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	wantPaymentRequired := func(t *testing.T, id string) {
		t.Helper()
		_, err := completeOrder.Execute(c, orderingdto.CompleteOrderInput{OrderID: id})
		var de *kernel.Error
		if !errors.As(err, &de) || de.Code() != "order.payment_required" || de.Kind() != kernel.KindFailedPrecondition {
			t.Fatalf("CompleteOrder(%s) err = %v, want order.payment_required (FailedPrecondition)", id, err)
		}
		o, err := orders.FindByID(c, order.ID(id))
		if err != nil || o.Status() != order.StatusConfirmed {
			t.Fatalf("after the refusal order %s = %v, %v; want still CONFIRMED", id, o, err)
		}
	}

	// runWorker starts apps/worker exactly as main does and returns its stop func.
	// The context carries the parent's deadline, so a failing test cannot leave it running.
	runWorker := func(t *testing.T, setting string) (stop func()) {
		t.Helper()
		t.Setenv("APP_POSTGRES_URL", url)
		t.Setenv("APP_LOG_LEVEL", "error")
		t.Setenv("APP_WORKER_CONSUMER_GROUP", "snap-"+runID+"-"+setting)
		if setting == "default" {
			// A worker started earlier in this test may have set it: the default cell runs without it.
			if prev, had := os.LookupEnv(requirePaymentEnv); had {
				if err := os.Unsetenv(requirePaymentEnv); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Setenv(requirePaymentEnv, prev) })
			}
		} else {
			t.Setenv(requirePaymentEnv, setting)
		}
		cmd := newRootCmd(config.Load)
		cmd.SetArgs(nil)
		wc, wcancel := context.WithCancel(c)
		done := make(chan error, 1)
		go func() { done <- cmd.ExecuteContext(wc) }()

		return func() {
			wcancel()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("worker exited with %v", err)
				}
			case <-time.After(20 * time.Second):
				t.Error("worker did not stop within 20s of cancel")
			}
		}
	}

	// Each cell makes its own order, so a failure in one cannot hide another: on an
	// implementation that ignores the setting, the first cell completes ITS order and the second
	// cell's order is still CONFIRMED when the switch-back assertion runs.
	t.Run("switch true: CONFIRMED order is refused", func(t *testing.T) {
		stop := runWorker(t, "true")
		defer stop()
		held := seed(t, "held")
		reserve(t, held)
		waitConfirmed(t, held)
		wantPaymentRequired(t, held)
	})

	t.Run("switch back to default: an order confirmed under true is still refused", func(t *testing.T) {
		// Confirmed by a worker running with the switch on; nothing touches it afterwards.
		stopOn := runWorker(t, "true")
		held := seed(t, "flipped")
		reserve(t, held)
		waitConfirmed(t, held)
		stopOn()

		// The switch goes back to the default and the worker restarts.
		stop := runWorker(t, "default")
		defer stop()
		wantPaymentRequired(t, held)

		// A new order confirmed under the default switch has no payment to wait for.
		id := seed(t, "plain")
		reserve(t, id)
		waitConfirmed(t, id)
		done, err := completeOrder.Execute(c, orderingdto.CompleteOrderInput{OrderID: id})
		if err != nil || done.Status != "COMPLETED" {
			t.Fatalf("CompleteOrder(%s) under the default switch = %+v, %v; want COMPLETED", id, done, err)
		}
		// And the held order is still refused after the worker handled a second order.
		wantPaymentRequired(t, held)
	})
}
