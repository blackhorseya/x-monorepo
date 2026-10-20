package eventbus_test

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	wsql "github.com/ThreeDotsLabs/watermill-sql/v4/pkg/sql"
	"github.com/ThreeDotsLabs/watermill/message"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/trace"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
	platformredis "github.com/blackhorseya/x-monorepo/pkgs/platform/redis"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/redis/redistest"
)

const drainTopic = "eventbustest.v1.Drain"

// A handler that is running when the router's context is cancelled keeps a
// live context, finishes its work and is acked: the worker's shutdown drains
// in-flight messages instead of aborting them and redelivering after
// restart. Both brokers, through the subscribers apps/worker uses.
func TestRouter_DrainsInFlightOnShutdown(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		db := drainDB(t)
		drainInFlight(t, func(group string) message.Subscriber {
			sub, err := eventbus.NewPostgresSubscriber(db, group, eventbus.FromBeginning, slog.Default())
			if err != nil {
				t.Fatal(err)
			}
			return sub
		}, func() message.Publisher {
			pub, err := eventbus.NewPostgresPublisher(db, slog.Default())
			if err != nil {
				t.Fatal(err)
			}
			return pub
		})
	})
	t.Run("redis", func(t *testing.T) {
		rdb, cleanup, err := platformredis.NewClient(config.Redis{URL: redistest.URL(t)})
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		if err := rdb.Del(context.Background(), drainTopic).Err(); err != nil {
			t.Fatal(err)
		}
		drainInFlight(t, func(group string) message.Subscriber {
			if err := eventbus.EnsureRedisGroup(context.Background(), rdb, drainTopic, group, eventbus.FromBeginning); err != nil {
				t.Fatal(err)
			}
			sub, err := eventbus.NewRedisSubscriber(rdb, group, eventbus.FromBeginning, slog.Default())
			if err != nil {
				t.Fatal(err)
			}
			return sub
		}, func() message.Publisher {
			pub, err := eventbus.NewRedisPublisher(rdb, slog.Default())
			if err != nil {
				t.Fatal(err)
			}
			return pub
		})
	})
}

// drainDB returns a pgtest DB with drainTopic's tables freshly created.
func drainDB(t *testing.T) *gorm.DB {
	t.Helper()
	c := context.Background()
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(telCleanup)
	db, cleanup, err := platformpg.NewDB(config.Postgres{URL: pgtest.URL(t)}, tel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	sqlDB, _ := db.DB()
	for _, q := range []string{
		"DROP TABLE IF EXISTS " + eventbus.TableName(drainTopic),
		"DROP TABLE IF EXISTS " + eventbus.OffsetsTableName(drainTopic),
		"CREATE SCHEMA IF NOT EXISTS eventbustest",
	} {
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
	if err := init.SubscribeInitialize(drainTopic); err != nil {
		t.Fatal(err)
	}

	return db
}

func drainInFlight(t *testing.T, subscriber func(group string) message.Subscriber, publisher func() message.Publisher) {
	t.Helper()
	const group = "drain"
	logs := &buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	tel := &otel.Providers{Tracer: trace.NewTracerProvider(), Propagator: propagation.TraceContext{}, Logger: logger}

	router, err := eventbus.NewRouter(logger, tel)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	var ctxErr atomic.Value // whether the handler's context was still live after the shutdown began
	err = eventbus.Register(router, subscriber(group), eventbus.ConsumerOptions{Poison: &capture{}, PoisonTopic: "drain.poison", MaxRetries: 1},
		eventbus.Subscription{Topic: drainTopic, Handler: eventbus.HandlerFunc(func(c context.Context, _ []byte) error {
			calls.Add(1)
			started <- struct{}{}
			<-release
			// What a use case would see: a DB call through c after SIGTERM.
			ctxErr.Store(c.Err() == nil)
			return nil
		})},
	)
	if err != nil {
		t.Fatal(err)
	}

	c, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- router.Run(c) }()
	<-router.Running()

	if err := publisher().Publish(drainTopic, message.NewMessage(watermill.NewUUID(), []byte("one"))); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("message never reached the handler")
	}

	// SIGTERM while the handler is running.
	cancel()
	select {
	case err := <-done:
		t.Fatalf("Run returned (%v) with a handler in flight", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after the handler finished")
	}
	if live, _ := ctxErr.Load().(bool); !live {
		t.Fatal("handler context was cancelled by the shutdown; a use case's DB call would fail mid-flight")
	}
	if logs.count("Discarding queued message") > 0 {
		t.Fatalf("subscriber discarded the in-flight message instead of waiting for its ack:\n%s", logs.all())
	}

	// A fresh router on the same group must not see the message again: it was acked.
	router2, err := eventbus.NewRouter(logger, tel)
	if err != nil {
		t.Fatal(err)
	}
	err = eventbus.Register(router2, subscriber(group), eventbus.ConsumerOptions{Poison: &capture{}, PoisonTopic: "drain.poison", MaxRetries: 1},
		eventbus.Subscription{Topic: drainTopic, Handler: eventbus.HandlerFunc(func(context.Context, []byte) error {
			calls.Add(1)
			return nil
		})},
	)
	if err != nil {
		t.Fatal(err)
	}
	c2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	_ = router2.Run(c2)
	if n := calls.Load(); n != 1 {
		t.Fatalf("handler calls = %d, want 1: the in-flight message was redelivered after restart", n)
	}
}

// A handler that outlives DrainTimeout is abandoned: its context is
// cancelled and Run returns, so a stuck handler cannot hold a pod past its
// termination grace period.
func TestRouter_DrainTimeoutAbandonsHandler(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&buffer{}, nil))
	tel := &otel.Providers{Tracer: trace.NewTracerProvider(), Propagator: propagation.TraceContext{}, Logger: logger}
	ch := eventbus.NewGoChannel(logger)
	defer func() { _ = ch.Close() }()
	router, err := eventbus.NewRouter(logger, tel)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	cancelled := make(chan struct{}, 1)
	err = eventbus.Register(router, ch, eventbus.ConsumerOptions{Poison: &capture{}, PoisonTopic: "t.poison", DrainTimeout: 200 * time.Millisecond},
		eventbus.Subscription{Topic: "t.stuck", Handler: eventbus.HandlerFunc(func(c context.Context, _ []byte) error {
			started <- struct{}{}
			<-c.Done() // stuck until the drain gives up on it
			cancelled <- struct{}{}
			return c.Err()
		})},
	)
	if err != nil {
		t.Fatal(err)
	}
	c, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- router.Run(c) }()
	<-router.Running()
	if err := ch.Publish("t.stuck", message.NewMessage(watermill.NewUUID(), nil)); err != nil {
		t.Fatal(err)
	}
	<-started

	shutdownAt := time.Now()
	cancel()
	select {
	case <-cancelled:
		if since := time.Since(shutdownAt); since < 200*time.Millisecond {
			t.Fatalf("handler cancelled after %s, before DrainTimeout", since)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler never cancelled")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}
