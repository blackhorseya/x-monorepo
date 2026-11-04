package main

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"

	inventorypg "github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	orderingpg "github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	paymentpg "github.com/blackhorseya/x-monorepo/pkgs/payment/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// ordering.order_history is a record, not a delivery channel: `worker prune` (outbox and inbox
// retention) must never touch it, however old its rows are (#427). The control rows prove the
// command really ran and really deleted: an old, fully-read outbox row and an old inbox row go,
// the history rows stay. --older-than must be positive, so 1ns is the smallest the real entry takes.
func TestPrune_KeepsOrderHistory(t *testing.T) {
	c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cfg := &config.AppConfig{Postgres: config.Postgres{URL: pgtest.URL(t)}, Log: config.Log{Level: "warn", Format: "text"}}
	tel, telCleanup, err := otel.New(cfg.OTel, cfg.Log)
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	db := pgtest.DB(t, cfg.Postgres, tel)
	for _, m := range []postgres.Migration{orderingpg.Migration(), inventorypg.Migration(), paymentpg.Migration()} {
		if err := postgres.Up(c, cfg.Postgres, m); err != nil {
			t.Fatal(err)
		}
	}
	const topic = "ordering.v1.OrderCreated"
	for _, tbl := range []string{eventbus.TableName(topic), eventbus.OffsetsTableName(topic), "ordering.inbox", "ordering.order_history"} {
		if err := db.WithContext(c).Exec("TRUNCATE " + tbl).Error; err != nil {
			t.Fatal(err)
		}
	}
	count := func(tbl string) int64 {
		var n int64
		if err := db.WithContext(c).Raw("SELECT count(*) FROM " + tbl).Scan(&n).Error; err != nil {
			t.Fatal(err)
		}
		return n
	}

	// History rows written the way the product writes them, with an old domain time.
	usd, _ := kernel.NewMoney(100, "USD")
	it, _ := order.NewLineItem("S", 1, usd)
	old := time.Now().UTC().Add(-90 * 24 * time.Hour)
	store := orderingpg.NewHistoryStore(db)
	for _, id := range []order.ID{"o-old-1", "o-old-2"} {
		o, err := order.New(order.Params{ID: id, CustomerID: "c-1", Items: []order.LineItem{it}, Now: old})
		if err != nil {
			t.Fatal(err)
		}
		h, err := order.NewHistoryEntry(o.PullEvents()[0], order.ActorSystem, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Append(c, h); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.WithContext(c).Exec(`UPDATE ordering.order_history SET occurred_at = ?`, old).Error; err != nil {
		t.Fatal(err)
	}

	// Controls: an old outbox row (no consumer group → needs --unconsumed) and an old inbox row.
	pub, err := eventbus.NewPostgresPublisher(db, tel.Logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.Publish(topic, message.NewMessage(watermill.NewUUID(), []byte("p"))); err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(c).Exec("UPDATE " + eventbus.TableName(topic) + " SET created_at = created_at - interval '90 days'").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(c).Exec("INSERT INTO ordering.inbox (message_id, consumed_at) VALUES (?, now() - interval '90 days')", watermill.NewUUID()).Error; err != nil {
		t.Fatal(err)
	}
	if count(eventbus.TableName(topic)) != 1 || count("ordering.inbox") != 1 || count("ordering.order_history") != 2 {
		t.Fatal("fixture is not what the test assumes")
	}

	var out bytes.Buffer
	cmd := newRootCmd(func() (*config.AppConfig, error) { return cfg, nil })
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"prune", "--older-than", "1ns", "--unconsumed"})
	if err := cmd.ExecuteContext(c); err != nil {
		t.Fatalf("prune: %v\n%s", err, out.String())
	}

	if count(eventbus.TableName(topic)) != 0 || count("ordering.inbox") != 0 {
		t.Fatalf("control rows survived prune (outbox=%d inbox=%d): the command did not run for ordering\n%s", count(eventbus.TableName(topic)), count("ordering.inbox"), out.String())
	}
	if n := count("ordering.order_history"); n != 2 {
		t.Fatalf("order_history has %d rows after prune, want 2: prune must not touch it", n)
	}
}
