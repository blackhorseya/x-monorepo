package eventbus_test

import (
	"context"
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

// An idle subscriber with WithPollInterval(50ms) sees a message published
// after its first empty poll well inside the library's 1s default: with the
// default the next poll would be ~700ms after the publish below.
func TestWithPollInterval_DeliversWithoutWaitingASecond(t *testing.T) {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
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
	for _, q := range []string{"DROP SCHEMA IF EXISTS eventbustest CASCADE", "CREATE SCHEMA eventbustest"} {
		if _, err := sqlDB.ExecContext(c, q); err != nil {
			t.Fatal(err)
		}
	}
	const topic = "eventbustest.v1.Poll"
	init, err := wsql.NewSubscriber(wsql.BeginnerFromStdSQL(sqlDB), wsql.SubscriberConfig{
		ConsumerGroup:  "init",
		SchemaAdapter:  wsql.DefaultPostgreSQLSchema{GenerateMessagesTableName: eventbus.TableName, GeneratePayloadType: func(string) string { return "BYTEA" }},
		OffsetsAdapter: wsql.DefaultPostgreSQLOffsetsAdapter{GenerateMessagesOffsetsTableName: eventbus.OffsetsTableName},
	}, watermill.NopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	if err := init.SubscribeInitialize(topic); err != nil {
		t.Fatal(err)
	}
	pub, err := eventbus.NewPostgresPublisher(db, tel.Logger)
	if err != nil {
		t.Fatal(err)
	}

	sub, err := eventbus.NewPostgresSubscriber(db, "g", eventbus.FromBeginning, tel.Logger, eventbus.WithPollInterval(50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	ch, err := sub.Subscribe(c, topic)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(300 * time.Millisecond) // the first poll finds nothing; the subscriber is idle
	published := time.Now()
	if err := pub.Publish(topic, message.NewMessage(watermill.NewUUID(), []byte("x"))); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-ch:
		msg.Ack()
		if d := time.Since(published); d > 500*time.Millisecond {
			t.Fatalf("delivered %s after publish, want well under the 1s library default", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("message not delivered")
	}
}
