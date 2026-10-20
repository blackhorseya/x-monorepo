package eventbus_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	wsql "github.com/ThreeDotsLabs/watermill-sql/v4/pkg/sql"
	"github.com/ThreeDotsLabs/watermill/message"
	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
	platformredis "github.com/blackhorseya/x-monorepo/pkgs/platform/redis"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/redis/redistest"
)

// metrics is a MeterProvider whose data is read on demand.
type metrics struct {
	reader *sdkmetric.ManualReader
	mp     *sdkmetric.MeterProvider
}

func newMetrics() *metrics {
	r := sdkmetric.NewManualReader()
	return &metrics{reader: r, mp: sdkmetric.NewMeterProvider(sdkmetric.WithReader(r))}
}

// collect returns every data point of the named metric, keyed by attribute set.
func (m *metrics) collect(t *testing.T, name string) map[attribute.Distinct]any {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := m.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[attribute.Distinct]any{}
	for _, sm := range rm.ScopeMetrics {
		for _, met := range sm.Metrics {
			if met.Name != name {
				continue
			}
			switch data := met.Data.(type) {
			case metricdata.Gauge[int64]:
				for _, dp := range data.DataPoints {
					out[dp.Attributes.Equivalent()] = dp.Value
				}
			case metricdata.Gauge[float64]:
				for _, dp := range data.DataPoints {
					out[dp.Attributes.Equivalent()] = dp.Value
				}
			case metricdata.Histogram[float64]:
				for _, dp := range data.DataPoints {
					out[dp.Attributes.Equivalent()] = dp.Count
				}
			default:
				t.Fatalf("%s: unexpected data type %T", name, met.Data)
			}
		}
	}
	return out
}

func key(kvs ...attribute.KeyValue) attribute.Distinct {
	set := attribute.NewSet(kvs...)
	return set.Equivalent()
}

func cursorKey(topic, group, broker string) attribute.Distinct {
	return key(attribute.String("topic", topic), attribute.String("consumer_group", group), attribute.String("broker", broker))
}

// Every attempt is one histogram sample with its result: the parked message
// counts as two failed attempts (first + one retry), not as the ack PoisonQueue
// returns to the router.
func TestInstruments_PerAttempt(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	tel := &otel.Providers{Tracer: sdktrace.NewTracerProvider(), Propagator: propagation.TraceContext{}, Logger: logger}
	m := newMetrics()
	inst, err := eventbus.NewInstruments(m.mp)
	if err != nil {
		t.Fatal(err)
	}
	ch := eventbus.NewGoChannel(logger)
	defer func() { _ = ch.Close() }()
	poison := &capture{}
	router, err := eventbus.NewRouter(logger, tel)
	if err != nil {
		t.Fatal(err)
	}
	okDone := make(chan struct{}, 1)
	handler := func(err error, done chan<- struct{}) eventbus.Handler {
		return eventbus.HandlerFunc(func(context.Context, []byte) error {
			if done != nil {
				done <- struct{}{}
			}
			return err
		})
	}
	err = eventbus.Register(router, ch, eventbus.ConsumerOptions{Poison: poison, PoisonTopic: "t.poison", MaxRetries: 1, Instruments: inst},
		eventbus.Subscription{Topic: "t.ok", Handler: handler(nil, okDone)},
		eventbus.Subscription{Topic: "t.drop", Handler: handler(eventbus.ErrDrop, nil)},
		eventbus.Subscription{Topic: "t.fail", Handler: handler(errors.New("db down"), nil)},
	)
	if err != nil {
		t.Fatal(err)
	}
	c, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = router.Run(c) }()
	<-router.Running()
	for _, topic := range []string{"t.ok", "t.drop", "t.fail"} {
		if err := ch.Publish(topic, message.NewMessage(watermill.NewUUID(), []byte(topic))); err != nil {
			t.Fatal(err)
		}
	}
	<-okDone
	deadline := time.Now().Add(5 * time.Second)
	for poison.len() < 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)

	got := m.collect(t, "eventbus.consumer.duration")
	for topic, want := range map[string]struct {
		result string
		n      uint64
	}{"t.ok": {"ok", 1}, "t.drop": {"dropped", 1}, "t.fail": {"failed", 2}} {
		if n := got[key(attribute.String("topic", topic), attribute.String("result", want.result))]; n != want.n {
			t.Fatalf("%s result=%s: %v samples, want %d (all: %v)", topic, want.result, n, want.n, got)
		}
	}
}

func TestLag_Postgres(t *testing.T) {
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
	for _, p := range []string{"one", "two", "three"} {
		if err := pub.Publish(startTopic, message.NewMessage(watermill.NewUUID(), []byte(p))); err != nil {
			t.Fatal(err)
		}
	}

	m := newMetrics()
	cur := eventbus.Cursor{Topic: startTopic, ConsumerGroup: "lagger"}
	reg, err := eventbus.Lag(m.mp, eventbus.LagOptions{DB: db, Postgres: []eventbus.Cursor{cur}, PoisonTopics: []string{startTopic}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Unregister() }()

	// A group that never subscribed is behind by the whole table.
	k := cursorKey(startTopic, "lagger", "postgres")
	time.Sleep(50 * time.Millisecond)
	if got := m.collect(t, "eventbus.consumer.lag")[k]; got != int64(3) {
		t.Fatalf("lag before subscribing = %v, want 3", got)
	}
	if age := m.collect(t, "eventbus.consumer.oldest_age")[k]; age == nil || age.(float64) <= 0 || age.(float64) > 60 {
		t.Fatalf("oldest_age = %v, want (0, 60]", age)
	}
	// The table doubles as a "poison" topic here: its row count is the backlog.
	if got := m.collect(t, "eventbus.poison.messages")[key(attribute.String("topic", startTopic))]; got != int64(3) {
		t.Fatalf("poison = %v, want 3", got)
	}

	// The subscriber commits the group's cursor once per batch, so the gauge
	// moves when the batch is done, not per ack: drain the three and it is 0.
	sub, err := eventbus.NewPostgresSubscriber(db, "lagger", eventbus.FromBeginning, tel.Logger)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	ch, err := sub.Subscribe(c, startTopic)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"one", "two", "three"} {
		if got, ok := receive(t, ch, 5*time.Second); !ok || got != want {
			t.Fatalf("receive = %q ok=%v, want %s", got, ok, want)
		}
	}
	waitLag(t, m, k, 0)
	if age := m.collect(t, "eventbus.consumer.oldest_age")[k]; age != float64(0) {
		t.Fatalf("oldest_age when caught up = %v, want 0", age)
	}
}

func TestLag_Redis(t *testing.T) {
	c := context.Background()
	rdb, cleanup, err := platformredis.NewClient(config.Redis{URL: redistest.URL(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := rdb.Del(c, startTopic).Err(); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	m := newMetrics()
	cur := eventbus.Cursor{Topic: startTopic, ConsumerGroup: "lagger"}
	reg, err := eventbus.Lag(m.mp, eventbus.LagOptions{Redis: rdb, RedisStreams: []eventbus.Cursor{cur}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Unregister() }()
	k := cursorKey(startTopic, "lagger", "redis")

	// No stream, then a stream without the group: nothing to report yet.
	if got := m.collect(t, "eventbus.consumer.lag")[k]; got != int64(0) {
		t.Fatalf("lag without stream = %v, want 0", got)
	}
	pub, err := eventbus.NewRedisPublisher(rdb, logger)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"one", "two", "three"} {
		if err := pub.Publish(startTopic, message.NewMessage(watermill.NewUUID(), []byte(p))); err != nil {
			t.Fatal(err)
		}
	}
	if got := m.collect(t, "eventbus.consumer.lag")[k]; got != int64(0) {
		t.Fatalf("lag without group = %v, want 0", got)
	}

	// Group at the beginning: everything is undelivered.
	if err := eventbus.EnsureRedisGroup(c, rdb, startTopic, "lagger", eventbus.FromBeginning); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if got := m.collect(t, "eventbus.consumer.lag")[k]; got != int64(3) {
		t.Fatalf("lag = %v, want 3", got)
	}
	if age := m.collect(t, "eventbus.consumer.oldest_age")[k]; age == nil || age.(float64) <= 0 || age.(float64) > 60 {
		t.Fatalf("oldest_age = %v, want (0, 60]", age)
	}

	// Delivered but not acked still counts (XINFO lag 2 + pending 1).
	peeked, err := rdb.XReadGroup(c, &goredis.XReadGroupArgs{Group: "lagger", Consumer: "peek", Streams: []string{startTopic, ">"}, Count: 1}).Result()
	if err != nil {
		t.Fatal(err)
	}
	if got := m.collect(t, "eventbus.consumer.lag")[k]; got != int64(3) {
		t.Fatalf("lag with one pending = %v, want 3", got)
	}
	if age := m.collect(t, "eventbus.consumer.oldest_age")[k]; age == nil || age.(float64) <= 0 {
		t.Fatalf("oldest_age with pending = %v, want > 0", age)
	}

	// Acking the peeked entry leaves two undelivered (XAUTOCLAIM would take
	// minutes to hand it to the subscriber); the subscriber drains those.
	if err := rdb.XAck(c, startTopic, "lagger", peeked[0].Messages[0].ID).Err(); err != nil {
		t.Fatal(err)
	}
	waitLag(t, m, k, 2)
	sub, err := eventbus.NewRedisSubscriber(rdb, "lagger", eventbus.FromBeginning, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	ch, err := sub.Subscribe(c, startTopic)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"two", "three"} {
		if got, ok := receive(t, ch, 5*time.Second); !ok || got != want {
			t.Fatalf("receive = %q ok=%v, want %s", got, ok, want)
		}
	}
	waitLag(t, m, k, 0)
}

// waitLag polls the gauge until it reads want (acks are asynchronous).
func waitLag(t *testing.T, m *metrics, k attribute.Distinct, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := m.collect(t, "eventbus.consumer.lag")[k]
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lag = %v, want %d", got, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
