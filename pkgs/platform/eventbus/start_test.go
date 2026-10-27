package eventbus_test

import (
	"context"
	"os"
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
	platformredis "github.com/blackhorseya/x-monorepo/pkgs/platform/redis"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/redis/redistest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Terminate()
	redistest.Terminate()
	os.Exit(code)
}

// A test-only topic in its own schema: platform must not import a BC's
// migrations (depguard), so the tables come from the library's own DDL.
const startTopic = "eventbustest.v1.Ping"

// receive waits for one message on ch (acking it) or reports none within d.
func receive(t *testing.T, ch <-chan *message.Message, d time.Duration) (string, bool) {
	t.Helper()
	select {
	case msg := <-ch:
		msg.Ack()
		return string(msg.Payload), true
	case <-time.After(d):
		return "", false
	}
}

func TestPostgresSubscriber_Start(t *testing.T) {
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
	for _, q := range []string{"DROP SCHEMA IF EXISTS eventbustest CASCADE", "CREATE SCHEMA eventbustest"} {
		if _, err := sqlDB.ExecContext(c, q); err != nil {
			t.Fatal(err)
		}
	}
	// Library DDL for the messages + offsets tables, as InitializeSchema would run it.
	init, err := wsql.NewSubscriber(wsql.BeginnerFromStdSQL(sqlDB), wsql.SubscriberConfig{
		ConsumerGroup:  "init",
		SchemaAdapter:  wsql.DefaultPostgreSQLSchema{GenerateMessagesTableName: eventbus.TableName, GeneratePayloadType: func(string) string { return "BYTEA" }},
		OffsetsAdapter: wsql.DefaultPostgreSQLOffsetsAdapter{GenerateMessagesOffsetsTableName: eventbus.OffsetsTableName},
	}, watermill.NopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	if err := init.SubscribeInitialize(startTopic); err != nil {
		t.Fatal(err)
	}

	pub, err := eventbus.NewPostgresPublisher(db, tel.Logger)
	if err != nil {
		t.Fatal(err)
	}
	publish := func(payload string) {
		t.Helper()
		if err := pub.Publish(startTopic, message.NewMessage(watermill.NewUUID(), []byte(payload))); err != nil {
			t.Fatal(err)
		}
	}
	open := func(group string, start eventbus.Start) (<-chan *message.Message, message.Subscriber) {
		t.Helper()
		sub, err := eventbus.NewPostgresSubscriber(db, group, start, tel.Logger)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sub.Close() })
		ch, err := sub.Subscribe(c, startTopic)
		if err != nil {
			t.Fatal(err)
		}
		return ch, sub
	}
	subscribe := func(group string, start eventbus.Start) <-chan *message.Message {
		t.Helper()
		ch, _ := open(group, start)
		return ch
	}

	publish("one")
	publish("two")

	// A new group from the beginning replays both.
	begin, beginSub := open("from-beginning", eventbus.FromBeginning)
	for _, want := range []string{"one", "two"} {
		if got, ok := receive(t, begin, 5*time.Second); !ok || got != want {
			t.Fatalf("beginning: got %q ok=%v, want %q", got, ok, want)
		}
	}

	// A new group from latest skips them and gets only what comes next.
	latest := subscribe("from-latest", eventbus.FromLatest)
	if got, ok := receive(t, latest, 1500*time.Millisecond); ok {
		t.Fatalf("latest replayed %q", got)
	}
	publish("three")
	if got, ok := receive(t, latest, 5*time.Second); !ok || got != "three" {
		t.Fatalf("latest: got %q ok=%v, want three", got, ok)
	}
	// The seed never moves an existing cursor: "from-beginning" still has its
	// own position and sees "three" as its third message, not as its first.
	if got, ok := receive(t, begin, 5*time.Second); !ok || got != "three" {
		t.Fatalf("beginning after latest: got %q ok=%v, want three", got, ok)
	}
	// Re-subscribing an existing group with FromLatest does not skip anything
	// either: "from-beginning" reopened as latest still continues from its cursor.
	// The first subscriber is closed first: two live subscribers of one group
	// share its messages (that is the point of a group), so with both polling
	// either could take "four" and the reopened one would see nothing. Msg.Ack
	// only signals the subscriber, which then commits the cursor; closing
	// before that commit rolls "three" back and it would be delivered again.
	var acked bool
	for deadline := time.Now().Add(5 * time.Second); !acked && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		row := sqlDB.QueryRowContext(c, `SELECT offset_acked >= (SELECT max("offset") FROM `+eventbus.TableName(startTopic)+`)
			FROM `+eventbus.OffsetsTableName(startTopic)+` WHERE consumer_group = 'from-beginning'`)
		if err := row.Scan(&acked); err != nil {
			t.Fatal(err)
		}
	}
	if !acked {
		t.Fatal("from-beginning never committed its cursor past three")
	}
	if err := beginSub.Close(); err != nil {
		t.Fatal(err)
	}
	publish("four")
	reopened := subscribe("from-beginning", eventbus.FromLatest)
	if got, ok := receive(t, reopened, 5*time.Second); !ok || got != "four" {
		t.Fatalf("reopened existing group: got %q ok=%v, want four", got, ok)
	}
}

func TestRedisSubscriber_Start(t *testing.T) {
	c := context.Background()
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	rdb, cleanup, err := platformredis.NewClient(config.Redis{URL: redistest.URL(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := rdb.Del(c, startTopic).Err(); err != nil {
		t.Fatal(err)
	}

	pub, err := eventbus.NewRedisPublisher(rdb, tel.Logger)
	if err != nil {
		t.Fatal(err)
	}
	publish := func(payload string) {
		t.Helper()
		if err := pub.Publish(startTopic, message.NewMessage(watermill.NewUUID(), []byte(payload))); err != nil {
			t.Fatal(err)
		}
	}
	subscribe := func(group string, start eventbus.Start) <-chan *message.Message {
		t.Helper()
		sub, err := eventbus.NewRedisSubscriber(rdb, group, start, tel.Logger)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sub.Close() })
		ch, err := sub.Subscribe(c, startTopic)
		if err != nil {
			t.Fatal(err)
		}
		return ch
	}

	publish("one")
	publish("two")
	begin := subscribe("from-beginning", eventbus.FromBeginning)
	for _, want := range []string{"one", "two"} {
		if got, ok := receive(t, begin, 5*time.Second); !ok || got != want {
			t.Fatalf("beginning: got %q ok=%v, want %q", got, ok, want)
		}
	}
	latest := subscribe("from-latest", eventbus.FromLatest)
	if got, ok := receive(t, latest, 1500*time.Millisecond); ok {
		t.Fatalf("latest replayed %q", got)
	}
	publish("three")
	if got, ok := receive(t, latest, 5*time.Second); !ok || got != "three" {
		t.Fatalf("latest: got %q ok=%v, want three", got, ok)
	}

	// EnsureRedisGroup pins "latest" to now: a message published before the
	// subscriber exists but after the group was created is still delivered.
	if err := eventbus.EnsureRedisGroup(c, rdb, startTopic, "ensured", eventbus.FromLatest); err != nil {
		t.Fatal(err)
	}
	publish("four")
	ensured := subscribe("ensured", eventbus.FromLatest)
	if got, ok := receive(t, ensured, 5*time.Second); !ok || got != "four" {
		t.Fatalf("ensured group: got %q ok=%v, want four", got, ok)
	}
	// Ensuring an existing group is a no-op.
	if err := eventbus.EnsureRedisGroup(c, rdb, startTopic, "ensured", eventbus.FromBeginning); err != nil {
		t.Fatal(err)
	}
}
