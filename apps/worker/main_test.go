package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"google.golang.org/protobuf/proto"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/consumer"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/events"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/usecase"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	orderingpg "github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/system"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

func TestRun_RequiresPostgres(t *testing.T) {
	t.Setenv("APP_POSTGRES_URL", "")
	if err := run(); err == nil {
		t.Fatal("missing postgres.url should fail")
	}
}

// Integration (TEST_DATABASE_URL, fatal when missing in CI): the whole loop —
// CreateOrder commits OrderCreated into the outbox table, the worker's router
// delivers it to a subscriber; an OrderCancellationRequested published into
// the same tables drives the real consumer, which cancels the order.
func TestWorker_EndToEnd(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("TEST_DATABASE_URL must be set in CI: a skipped integration test would be a false green")
		}
		t.Skip("TEST_DATABASE_URL not set")
	}

	c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
	if err := postgres.Up(c, pgCfg, orderingpg.Migration()); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"ordering.orders", "ordering.events_v1_ordercreated", "ordering.events_v1_ordercancelled", "ordering.events_v1_ordercancellationrequested", "ordering.events_poison"} {
		if err := db.WithContext(c).Exec("TRUNCATE " + tbl + " CASCADE").Error; err != nil {
			t.Fatal(err)
		}
	}

	// The producing side, exactly as apps/rpc wires it.
	logger := tel.Logger
	repo := ordering.ProvideOrderRepository(db, logger)
	tx := ordering.ProvideTransactor(db)
	publisher, pubCleanup, err := eventbus.ProvidePublisher(db, logger, tel)
	if err != nil {
		t.Fatal(err)
	}
	defer pubCleanup()
	evPub := events.NewPublisher(publisher)
	clock := system.NewClock()
	create := usecase.NewCreateOrder(repo, tx, evPub, system.NewUUIDGenerator(), clock)
	get := usecase.NewGetOrder(repo)
	cancelOrder := consumer.NewOrderCancellationHandler(usecase.NewCancelOrder(repo, tx, evPub, clock))

	// The worker, with one extra subscription so the test can observe OrderCreated.
	created := make(chan *orderingv1.OrderCreated, 1)
	subs := append(ordering.ProvideSubscriptions(cancelOrder), eventbus.Subscription{
		Topic: events.TopicOrderCreated,
		Handler: eventbus.HandlerFunc(func(_ context.Context, payload []byte) error {
			var ev orderingv1.OrderCreated
			if err := proto.Unmarshal(payload, &ev); err != nil {
				return fmt.Errorf("%w: %w", eventbus.ErrDrop, err)
			}
			created <- &ev
			return nil
		}),
	})
	group := fmt.Sprintf("test-%d", time.Now().UnixNano())
	w, err := NewWorker(config.Worker{ConsumerGroup: group}, db, logger, tel, publisher, subs)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(c)
	done := make(chan error, 1)
	go func() { done <- w.Run(runCtx) }()

	out, err := create.Execute(c, dto.CreateOrderInput{
		CustomerID: "c-1",
		Items:      []dto.LineItem{{SKU: "S", Quantity: 1, UnitPrice: dto.Money{AmountMinor: 100, Currency: "USD"}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case ev := <-created:
		if ev.GetOrderId() != out.ID || ev.GetCustomerId() != "c-1" || ev.GetTotal().GetAmountMinor() != 100 {
			t.Fatalf("OrderCreated = %v, want order %s", ev, out.ID)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("OrderCreated was not delivered")
	}

	// Another BC asks for a cancellation: same tables, same router, real handler.
	payload, _ := proto.Marshal(&orderingv1.OrderCancellationRequested{OrderId: out.ID, Reason: "payment timeout"})
	if err := publisher.Publish(consumer.Topic, message.NewMessageWithContext(c, watermill.NewUUID(), payload)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		got, err := get.Execute(c, dto.GetOrderInput{OrderID: out.ID})
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == "CANCELLED" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("order %s still %s after OrderCancellationRequested", out.ID, got.Status)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Cancelling raised OrderCancelled in the outbox too, and the group's offsets advanced.
	var n int64
	if err := db.WithContext(c).Raw("SELECT count(*) FROM ordering.events_v1_ordercancelled").Scan(&n).Error; err != nil || n != 1 {
		t.Fatalf("OrderCancelled rows = %d err=%v, want 1", n, err)
	}
	if err := db.WithContext(c).Raw("SELECT offset_acked FROM ordering.events_v1_ordercancellationrequested_offsets WHERE consumer_group = ?", group).Scan(&n).Error; err != nil || n != 1 {
		t.Fatalf("offset_acked = %d err=%v, want 1", n, err)
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
