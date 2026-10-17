package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"google.golang.org/protobuf/proto"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory"
	inventoryconsumer "github.com/blackhorseya/x-monorepo/pkgs/inventory/adapters/consumer"
	inventoryevents "github.com/blackhorseya/x-monorepo/pkgs/inventory/adapters/events"
	inventorydto "github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
	inventorypg "github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	inventorysys "github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/system"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering"
	orderingconsumer "github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/consumer"
	orderingevents "github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/events"
	orderingdto "github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	orderingpg "github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	orderingsys "github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/system"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

func TestRun_RequiresPostgres(t *testing.T) {
	t.Setenv("APP_POSTGRES_URL", "")
	if err := run(); err == nil {
		t.Fatal("missing postgres.url should fail")
	}
}

// Integration (TEST_DATABASE_URL, fatal when missing in CI): the whole loop
// across both BCs, through the real handlers and the real Postgres tables.
//
//	order A (stocked)   OrderCreated → inventory RESERVED → OrderCancellationRequested
//	                    (published by the test) → ordering CANCELLED → OrderCancelled
//	                    → inventory RELEASED
//	order B (unstocked) OrderCreated → inventory REJECTED → OrderCancellationRequested
//	                    (published by inventory) → ordering CANCELLED → OrderCancelled
//	order C (cancel first) OrderCancelled (published by the test, before any OrderCreated)
//	                    → inventory CANCELLED tombstone → late OrderCreated reserves nothing
//	                    → inventory: nothing to release (dropped, not poisoned)
func TestWorker_EndToEnd(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("TEST_DATABASE_URL must be set in CI: a skipped integration test would be a false green")
		}
		t.Skip("TEST_DATABASE_URL not set")
	}

	c, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pgCfg := config.Postgres{URL: url}
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	db, dbCleanup, err := postgres.NewDB(pgCfg, tel)
	if err != nil {
		t.Fatal(err)
	}
	defer dbCleanup()
	for _, m := range []postgres.Migration{orderingpg.Migration(), inventorypg.Migration()} {
		if err := postgres.Up(c, pgCfg, m); err != nil {
			t.Fatal(err)
		}
	}
	for _, tbl := range []string{
		"ordering.orders", "ordering.events_v1_ordercreated", "ordering.events_v1_ordercancelled",
		"ordering.events_v1_ordercancellationrequested", "ordering.events_poison",
		"inventory.stocks", "inventory.reservations", "inventory.events_v1_stockreserved",
		"inventory.events_v1_stockreleased", "inventory.events_poison",
	} {
		if err := db.WithContext(c).Exec("TRUNCATE " + tbl + " CASCADE").Error; err != nil {
			t.Fatal(err)
		}
	}

	// Both BCs, exactly as apps/rpc + apps/worker wire them.
	logger := tel.Logger
	publisher, pubCleanup, err := eventbus.ProvidePublisher(db, logger, tel)
	if err != nil {
		t.Fatal(err)
	}
	defer pubCleanup()

	rec, err := usecasex.NewRecorder(tel.Meter)
	if err != nil {
		t.Fatal(err)
	}

	orders := ordering.ProvideOrderRepository(db, logger)
	otx := ordering.ProvideTransactor(db)
	oev := orderingevents.NewPublisher(publisher)
	oclock := orderingsys.NewClock()
	createOrder := ordering.ProvideCreateOrder(orders, otx, oev, orderingsys.NewUUIDGenerator(), oclock, rec)
	getOrder := ordering.ProvideGetOrder(orders, rec)
	orderingSubs := ordering.ProvideSubscriptions(orderingconsumer.NewOrderCancellationHandler(
		ordering.ProvideCancelOrder(orders, otx, oev, oclock, rec)))

	stocks := inventory.ProvideStockRepository(db, logger)
	reservations := inventory.ProvideReservationRepository(db)
	itx := inventory.ProvideTransactor(db)
	iev := inventoryevents.NewPublisher(publisher)
	iclock := inventorysys.NewClock()
	setStock := inventory.ProvideSetStock(stocks, itx, iclock, rec)
	getStock := inventory.ProvideGetStock(stocks, rec)
	getReservation := inventory.ProvideGetReservation(reservations, rec)
	inventorySubs := inventory.ProvideSubscriptions(
		inventoryconsumer.NewOrderCreatedHandler(inventory.ProvideReserveStock(reservations, stocks, itx, iev, iclock, rec)),
		inventoryconsumer.NewOrderCancelledHandler(inventory.ProvideReleaseStock(reservations, stocks, itx, iev, iclock, rec)),
	)

	group := fmt.Sprintf("test-%d", time.Now().UnixNano())
	w, err := NewWorker(config.Worker{ConsumerGroup: group}, db, logger, tel, publisher, orderingSubs, inventorySubs)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(c)
	done := make(chan error, 1)
	go func() { done <- w.Run(runCtx) }()

	// waitFor polls until cond is true or the deadline passes.
	waitFor := func(what string, cond func() (bool, error)) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			ok, err := cond()
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			if ok {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	orderIs := func(orderID, status string) func() (bool, error) {
		return func() (bool, error) {
			o, err := getOrder.Execute(c, orderingdto.GetOrderInput{OrderID: orderID})
			return err == nil && o.Status == status, err
		}
	}
	reservationIs := func(orderID, status string) func() (bool, error) {
		return func() (bool, error) {
			r, err := getReservation.Execute(c, inventorydto.GetReservationInput{OrderID: orderID})
			if errors.Is(err, reservation.ErrNotFound) {
				return false, nil
			}
			return err == nil && r.Status == status, err
		}
	}

	// ── order A: stocked → RESERVED → cancelled by another BC → RELEASED ──
	if _, err := setStock.Execute(c, inventorydto.SetStockInput{SKU: "S", OnHand: 5}); err != nil {
		t.Fatal(err)
	}
	a, err := createOrder.Execute(c, orderingdto.CreateOrderInput{
		CustomerID: "c-1",
		Items:      []orderingdto.LineItem{{SKU: "S", Quantity: 2, UnitPrice: orderingdto.Money{AmountMinor: 100, Currency: "USD"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor("reservation A RESERVED", reservationIs(a.ID, "RESERVED"))
	if s, _ := getStock.Execute(c, inventorydto.GetStockInput{SKU: "S"}); s.Reserved != 2 || s.Available != 3 {
		t.Fatalf("stock after reserve = %+v", s)
	}

	payload, _ := proto.Marshal(&orderingv1.OrderCancellationRequested{OrderId: a.ID, Reason: "payment timeout"})
	msg := message.NewMessageWithContext(c, watermill.NewUUID(), payload)
	msg.Metadata.Set(eventbus.MetaKey, a.ID)
	if err := publisher.Publish(orderingconsumer.Topic, msg); err != nil {
		t.Fatal(err)
	}
	waitFor("order A CANCELLED", orderIs(a.ID, "CANCELLED"))
	waitFor("reservation A RELEASED", reservationIs(a.ID, "RELEASED"))
	if s, _ := getStock.Execute(c, inventorydto.GetStockInput{SKU: "S"}); s.Reserved != 0 || s.Available != 5 {
		t.Fatalf("stock after release = %+v", s)
	}

	// ── order B: unstocked SKU → REJECTED → inventory asks ordering to cancel ──
	b, err := createOrder.Execute(c, orderingdto.CreateOrderInput{
		CustomerID: "c-1",
		Items:      []orderingdto.LineItem{{SKU: "NOPE", Quantity: 1, UnitPrice: orderingdto.Money{AmountMinor: 100, Currency: "USD"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor("order B CANCELLED by inventory", orderIs(b.ID, "CANCELLED"))
	r, err := getReservation.Execute(c, inventorydto.GetReservationInput{OrderID: b.ID})
	if err != nil || r.Status != "REJECTED" || len(r.ShortfallSKUs) != 1 || r.ShortfallSKUs[0] != "NOPE" {
		t.Fatalf("reservation B = %+v, %v", r, err)
	}

	// ── order C: OrderCancelled overtakes OrderCreated → tombstone, late create reserves nothing ──
	cID := "c-" + watermill.NewUUID()
	cancelled, _ := proto.Marshal(&orderingv1.OrderCancelled{OrderId: cID, CustomerId: "c-1"})
	msg = message.NewMessageWithContext(c, watermill.NewUUID(), cancelled)
	msg.Metadata.Set(eventbus.MetaKey, cID)
	if err := publisher.Publish(inventoryconsumer.TopicOrderCancelled, msg); err != nil {
		t.Fatal(err)
	}
	waitFor("reservation C CANCELLED (tombstone)", reservationIs(cID, "CANCELLED"))

	created, _ := proto.Marshal(&orderingv1.OrderCreated{OrderId: cID, CustomerId: "c-1", Items: []*orderingv1.OrderCreatedItem{{Sku: "S", Quantity: 3}}})
	msg = message.NewMessageWithContext(c, watermill.NewUUID(), created)
	msg.Metadata.Set(eventbus.MetaKey, cID)
	if err := publisher.Publish(inventoryconsumer.TopicOrderCreated, msg); err != nil {
		t.Fatal(err)
	}
	// Consumption of the late OrderCreated shows only in the cursor: a, b and c
	// is three OrderCreated on the inventory group.
	waitFor("inventory consumed the late OrderCreated", func() (bool, error) {
		var n int64
		err := db.WithContext(c).Raw("SELECT coalesce(max(offset_acked), 0) FROM ordering.events_v1_ordercreated_offsets WHERE consumer_group = ?", group+".inventory").Scan(&n).Error
		return n >= 3, err
	})
	if r, err := getReservation.Execute(c, inventorydto.GetReservationInput{OrderID: cID}); err != nil || r.Status != "CANCELLED" || len(r.Lines) != 0 {
		t.Fatalf("reservation C after late create = %+v, %v", r, err)
	}
	if s, _ := getStock.Execute(c, inventorydto.GetStockInput{SKU: "S"}); s.Reserved != 0 {
		t.Fatalf("late create reserved stock: %+v", s)
	}

	// Every hop left its row in the outbox tables (counted by aggregate key, so
	// traffic from a locally running rpc does not interfere); nothing was
	// poisoned, and each BC's consumer group advanced on its own cursor.
	counts := map[string]int64{
		"ordering.events_v1_ordercreated":               3, // a, b, and c (published by the test)
		"ordering.events_v1_ordercancellationrequested": 2, // one by the test, one by inventory
		"ordering.events_v1_ordercancelled":             3, // a, b, and c (published by the test)
		"inventory.events_v1_stockreserved":             1,
		"inventory.events_v1_stockreleased":             1,
		"ordering.events_poison":                        0,
		"inventory.events_poison":                       0,
	}
	// OrderCancelled for B reaches inventory too, on a REJECTED reservation: a
	// domain error, dropped (ack) — not retried, not poisoned. Wait for that ack.
	waitFor("inventory acked every OrderCancelled", func() (bool, error) {
		var n int64
		err := db.WithContext(c).Raw("SELECT coalesce(max(offset_acked), 0) FROM ordering.events_v1_ordercancelled_offsets WHERE consumer_group = ?", group+".inventory").Scan(&n).Error
		return n >= 3, err
	})
	for tbl, want := range counts {
		var n int64
		if err := db.WithContext(c).Raw("SELECT count(*) FROM "+tbl+" WHERE metadata->>'key' IN (?, ?, ?)", a.ID, b.ID, cID).Scan(&n).Error; err != nil || n != want {
			t.Fatalf("%s rows = %d err=%v, want %d", tbl, n, err, want)
		}
	}
	var n int64
	if err := db.WithContext(c).Raw("SELECT offset_acked FROM ordering.events_v1_ordercancellationrequested_offsets WHERE consumer_group = ?", group+".ordering").Scan(&n).Error; err != nil || n < 2 {
		t.Fatalf("ordering offset_acked = %d err=%v, want >= 2", n, err)
	}

	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("worker did not stop")
	}
}
