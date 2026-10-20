package eventbus_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	wsql "github.com/ThreeDotsLabs/watermill-sql/v4/pkg/sql"
	"github.com/ThreeDotsLabs/watermill/message"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

const poisonTestTopic = "poisontest.v1.Ping"

// Integration (pgtest): a handler that fails parks the message on the BC's
// poison table; Requeue puts it back on its topic (same uuid, poison keys
// gone) where the same group consumes it again; Drop deletes it.
func TestPoison_ListRequeueDrop(t *testing.T) {
	c := context.Background()
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	db, cleanup, err := platformpg.NewDB(config.Postgres{URL: pgtest.URL(t)}, tel)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	sqlDB, _ := db.DB()
	for _, q := range []string{"DROP SCHEMA IF EXISTS poisontest CASCADE", "CREATE SCHEMA poisontest"} {
		if _, err := sqlDB.ExecContext(c, q); err != nil {
			t.Fatal(err)
		}
	}
	init, err := wsql.NewSubscriber(wsql.BeginnerFromStdSQL(sqlDB), wsql.SubscriberConfig{
		ConsumerGroup:  "init",
		SchemaAdapter:  wsql.DefaultPostgreSQLSchema{GenerateMessagesTableName: eventbus.TableName, GeneratePayloadType: func(string) string { return "BYTEA" }},
		OffsetsAdapter: wsql.DefaultPostgreSQLOffsetsAdapter{GenerateMessagesOffsetsTableName: eventbus.OffsetsTableName},
	}, watermill.NopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	poisonTopic := eventbus.PoisonTopic("poisontest")
	for _, topic := range []string{poisonTestTopic, poisonTopic} {
		if err := init.SubscribeInitialize(topic); err != nil {
			t.Fatal(err)
		}
	}
	pub, err := eventbus.NewPostgresPublisher(db, tel.Logger)
	if err != nil {
		t.Fatal(err)
	}

	// The handler fails while broken is set; every attempt is reported.
	var broken atomic.Bool
	broken.Store(true)
	attempts := make(chan string, 16)
	router, err := eventbus.NewRouter(tel.Logger, tel)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := eventbus.NewPostgresSubscriber(db, "g", eventbus.FromBeginning, tel.Logger)
	if err != nil {
		t.Fatal(err)
	}
	err = eventbus.Register(router, sub, eventbus.ConsumerOptions{Poison: pub, PoisonTopic: poisonTopic, MaxRetries: 1},
		eventbus.Subscription{Topic: poisonTestTopic, Handler: eventbus.HandlerFunc(func(_ context.Context, payload []byte) error {
			attempts <- string(payload)
			if broken.Load() {
				return errors.New("boom: db down")
			}
			return nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(c)
	defer cancel()
	go func() { _ = router.Run(runCtx) }()
	<-router.Running()

	next := func() string {
		select {
		case p := <-attempts:
			return p
		case <-time.After(10 * time.Second):
			t.Fatal("no handler attempt")
			return ""
		}
	}
	waitPoison := func(want int) []eventbus.PoisonMessage {
		deadline := time.Now().Add(10 * time.Second)
		for {
			got, err := eventbus.ListPoison(c, db, poisonTopic, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) == want || time.Now().After(deadline) {
				if len(got) != want {
					t.Fatalf("poison rows = %d, want %d", len(got), want)
				}
				return got
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// 1. Fail → parked, with the failure described in metadata.
	msg := message.NewMessage(watermill.NewUUID(), []byte("one"))
	msg.Metadata.Set("traceparent", "00-abc-def-01")
	if err := pub.Publish(poisonTestTopic, msg); err != nil {
		t.Fatal(err)
	}
	next()
	next() // MaxRetries 1 = two attempts
	parked := waitPoison(1)
	p := parked[0]
	if p.UUID != msg.UUID || string(p.Payload) != "one" || p.Topic != poisonTestTopic || p.Handler != poisonTestTopic {
		t.Fatalf("parked = %+v", p)
	}
	if !strings.Contains(p.Reason, "boom") || p.Metadata.Get("traceparent") != "00-abc-def-01" {
		t.Fatalf("parked reason=%q metadata=%v", p.Reason, p.Metadata)
	}

	// 2. Requeue → the fixed handler gets it again, same uuid, no poison keys.
	broken.Store(false)
	n, err := eventbus.Requeue(c, db, poisonTopic, p.Offset)
	if err != nil || n != 1 {
		t.Fatalf("requeue = %d, %v", n, err)
	}
	if got := next(); got != "one" {
		t.Fatalf("requeued payload = %q", got)
	}
	waitPoison(0)
	var meta string
	err = sqlDB.QueryRowContext(c, `SELECT metadata::text FROM `+eventbus.TableName(poisonTestTopic)+` WHERE uuid = $1 ORDER BY "offset" DESC LIMIT 1`, msg.UUID).Scan(&meta)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(meta, "poisoned") || !strings.Contains(meta, "traceparent") {
		t.Fatalf("requeued metadata = %s", meta)
	}
	// Requeueing a row that is gone is not an error.
	if n, err := eventbus.Requeue(c, db, poisonTopic, p.Offset); err != nil || n != 0 {
		t.Fatalf("requeue again = %d, %v", n, err)
	}

	// 3. Drop: two parked, drop one by offset, then the rest.
	broken.Store(true)
	for _, payload := range []string{"two", "three"} {
		if err := pub.Publish(poisonTestTopic, message.NewMessage(watermill.NewUUID(), []byte(payload))); err != nil {
			t.Fatal(err)
		}
		next()
		next()
	}
	parked = waitPoison(2)
	if n, err := eventbus.Drop(c, db, poisonTopic, parked[0].Offset); err != nil || n != 1 {
		t.Fatalf("drop one = %d, %v", n, err)
	}
	if left := waitPoison(1); string(left[0].Payload) != "three" {
		t.Fatalf("left = %q", left[0].Payload)
	}
	if n, err := eventbus.Drop(c, db, poisonTopic); err != nil || n != 1 {
		t.Fatalf("drop all = %d, %v", n, err)
	}
	waitPoison(0)
	if l, err := eventbus.ListPoison(c, db, poisonTopic, 1); err != nil || len(l) != 0 {
		t.Fatalf("list after drop = %v, %v", l, err)
	}
}
