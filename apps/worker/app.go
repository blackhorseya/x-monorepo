package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/ThreeDotsLabs/watermill/message"
	goredis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
)

// Worker owns the router. Adding a BC: wire.go gets its ProviderSet, NewWorker
// a parameter for its Subscriptions and a Register call with its poison topic.
type Worker struct {
	router *message.Router
	logger *slog.Logger
}

// NewWorker registers every BC's subscriptions on one router. Each BC reads
// through its own subscriber with its own consumer group
// ("<worker.consumer_group>.<bc>"): two BCs subscribed to the same topic each
// get every message instead of splitting it, while replicas of this process
// (same worker.consumer_group) still share the work. Failed messages of a BC
// go to that BC's poison topic, always in Postgres.
//
// worker.broker decides what the handlers read:
//
//   - postgres: the outbox tables themselves (no broker to run).
//   - redis: a forwarder in this process copies every consumed topic's outbox
//     table into the Redis Stream of the same name (consumer group
//     ForwarderGroup, shared by all replicas so each message is forwarded
//     once), and the handlers consume the streams. The outbox stays the
//     write path, so "event committed iff state committed" still holds; what
//     changes is that after the forwarder acks, the message lives only in
//     Redis (run it with persistence) and that SQL → Redis is at-least-once.
func NewWorker(
	cfg config.Worker,
	db *gorm.DB,
	rdb goredis.UniversalClient,
	logger *slog.Logger,
	tel *otel.Providers,
	publisher message.Publisher,
	orderingSubs ordering.Subscriptions,
	inventorySubs inventory.Subscriptions,
) (*Worker, error) {
	router, err := eventbus.NewRouter(logger, tel)
	if err != nil {
		return nil, err
	}
	bcs := []struct {
		schema string
		subs   []eventbus.Subscription
	}{
		{ordering.Schema, orderingSubs},
		{inventory.Schema, inventorySubs},
	}

	// subscriber builds the per-BC subscriber the handlers read from.
	var subscriber func(group string) (message.Subscriber, error)
	switch cfg.Broker {
	case config.BrokerPostgres, "":
		subscriber = func(group string) (message.Subscriber, error) {
			return eventbus.NewPostgresSubscriber(db, group, logger)
		}
	case config.BrokerRedis:
		if rdb == nil {
			return nil, errors.New("worker.broker is redis but there is no redis client (redis.url)")
		}
		if err := addForwarders(router, db, rdb, logger, bcs); err != nil {
			return nil, err
		}
		subscriber = func(group string) (message.Subscriber, error) {
			return eventbus.NewRedisSubscriber(rdb, group, logger)
		}
	default:
		return nil, fmt.Errorf("unknown worker.broker %q", cfg.Broker)
	}

	for _, bc := range bcs {
		sub, err := subscriber(cfg.ConsumerGroup + "." + bc.schema)
		if err != nil {
			return nil, err
		}
		err = eventbus.Register(router, sub, eventbus.ConsumerOptions{
			Poison:      publisher,
			PoisonTopic: eventbus.PoisonTopic(bc.schema),
		}, bc.subs...)
		if err != nil {
			return nil, err
		}
	}

	return &Worker{router: router, logger: logger}, nil
}

// ForwarderGroup is the Postgres consumer group of the outbox → Redis
// forwarder. It is fixed on purpose: forwarding happens once per message no
// matter how many worker.consumer_groups consume the streams.
const ForwarderGroup = "forwarder"

// addForwarders adds one passthrough handler per distinct consumed topic:
// outbox table (Postgres, ForwarderGroup) → Redis Stream of the same name.
// Per-topic passthrough keeps the existing tables and migrations; Watermill's
// envelope-based forwarder component would need a table of its own.
func addForwarders(
	router *message.Router,
	db *gorm.DB,
	rdb goredis.UniversalClient,
	logger *slog.Logger,
	bcs []struct {
		schema string
		subs   []eventbus.Subscription
	},
) error {
	sub, err := eventbus.NewPostgresSubscriber(db, ForwarderGroup, logger)
	if err != nil {
		return err
	}
	pub, err := eventbus.NewRedisPublisher(rdb, logger)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, bc := range bcs {
		for _, s := range bc.subs {
			if seen[s.Topic] {
				continue
			}
			seen[s.Topic] = true
			router.AddHandler("forward:"+s.Topic, s.Topic, sub, s.Topic, pub, message.PassthroughHandler)
		}
	}

	return nil
}

// Run blocks until c is done, then drains in-flight handlers.
func (w *Worker) Run(c context.Context) error {
	w.logger.Info("worker started")
	defer w.logger.Info("worker stopped")

	return w.router.Run(c)
}
