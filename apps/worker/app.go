package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/metric"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/buildinfo"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
)

// Worker owns the router. Adding a BC: wire.go gets its ProviderSet, NewWorker
// a parameter for its Subscriptions and a Register call with its poison topic.
type Worker struct {
	router *message.Router
	logger *slog.Logger
	// gauges are the callback registrations observed while Run runs: the
	// eventbus lag gauges and each BC's funnel gauges (orders / reservations per status).
	gauges []metric.Registration
}

// NewWorker registers every BC's subscriptions on one router. Each BC reads
// through its own subscriber with its own consumer group
// ("<worker.consumer_group>.<bc>"): two BCs subscribed to the same topic each
// get every message instead of splitting it, while replicas of this process
// (same worker.consumer_group) still share the work. Failed messages of a BC
// go to that BC's poison topic, always in Postgres.
//
// worker.handler_timeout bounds each handler attempt (ConsumerOptions
// .HandlerTimeout); a use case that hangs on the database fails that attempt
// instead of holding the message forever.
//
// Metrics (platform/eventbus): every handler attempt is a sample of
// eventbus.consumer.duration, and eventbus.Lag reports each cursor's backlog —
// the BC groups on whatever they read from, the forwarder's Postgres cursor in
// redis mode, and each BC's poison table — plus, with a database, the size and
// retention state of every outbox table and inbox (what `worker prune` deletes
// or is held back from deleting).
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
	orderingGauges ordering.Gauges,
	inventoryGauges inventory.Gauges,
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

	start := eventbus.FromBeginning
	if cfg.StartFrom == config.StartFromLatest {
		start = eventbus.FromLatest
	}
	instruments, err := eventbus.NewInstruments(tel.Meter)
	if err != nil {
		return nil, err
	}
	lag := eventbus.LagOptions{DB: db, Redis: rdb}
	for _, bc := range bcs {
		lag.PoisonTopics = append(lag.PoisonTopics, eventbus.PoisonTopic(bc.schema))
	}
	if db != nil {
		// Retention gauges for what `worker prune` manages: every outbox table
		// of every BC (found in the catalogue, so published-only topics count)
		// and each BC's inbox.
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, bc := range bcs {
			tables, err := eventbus.TopicTables(c, db, bc.schema)
			if err != nil {
				return nil, err
			}
			lag.OutboxTables = append(lag.OutboxTables, tables...)
			lag.InboxSchemas = append(lag.InboxSchemas, bc.schema)
		}
	}

	// subscriber builds the per-BC subscriber the handlers read from.
	var subscriber func(group string) (message.Subscriber, error)
	switch cfg.Broker {
	case config.BrokerPostgres, "":
		subscriber = func(group string) (message.Subscriber, error) {
			return eventbus.NewPostgresSubscriber(db, group, start, logger, eventbus.WithPollInterval(cfg.PollInterval))
		}
		for _, bc := range bcs {
			for _, s := range bc.subs {
				lag.Postgres = append(lag.Postgres, eventbus.Cursor{Topic: s.Topic, ConsumerGroup: cfg.ConsumerGroup + "." + bc.schema})
			}
		}
	case config.BrokerRedis:
		if rdb == nil {
			return nil, errors.New("worker.broker is redis but there is no redis client (redis.url)")
		}
		// Stream groups are created here, before the forwarder can run: with
		// "latest" a group created lazily on Subscribe would miss whatever the
		// forwarder copied in between.
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, bc := range bcs {
			for _, s := range bc.subs {
				if err := eventbus.EnsureRedisGroup(c, rdb, s.Topic, cfg.ConsumerGroup+"."+bc.schema, start); err != nil {
					return nil, err
				}
			}
		}
		forwarded, err := addForwarders(router, db, rdb, start, cfg.PollInterval, logger, bcs)
		if err != nil {
			return nil, err
		}
		for _, topic := range forwarded {
			lag.Postgres = append(lag.Postgres, eventbus.Cursor{Topic: topic, ConsumerGroup: ForwarderGroup})
		}
		subscriber = func(group string) (message.Subscriber, error) {
			return eventbus.NewRedisSubscriber(rdb, group, start, logger)
		}
		for _, bc := range bcs {
			for _, s := range bc.subs {
				lag.RedisStreams = append(lag.RedisStreams, eventbus.Cursor{Topic: s.Topic, ConsumerGroup: cfg.ConsumerGroup + "." + bc.schema})
			}
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
			Poison:         publisher,
			PoisonTopic:    eventbus.PoisonTopic(bc.schema),
			Instruments:    instruments,
			HandlerTimeout: cfg.HandlerTimeout,
		}, bc.subs...)
		if err != nil {
			return nil, err
		}
	}
	reg, err := eventbus.Lag(tel.Meter, lag)
	if err != nil {
		return nil, err
	}
	w := &Worker{router: router, logger: logger, gauges: []metric.Registration{reg}}
	// The saga funnel (ordering_orders / inventory_reservations by status): read
	// from the database on every scrape like the lag gauges, so only with one.
	if db != nil {
		for _, g := range []interface {
			Register(metric.MeterProvider) (metric.Registration, error)
		}{orderingGauges, inventoryGauges} {
			r, err := g.Register(tel.Meter)
			if err != nil {
				return nil, err
			}
			w.gauges = append(w.gauges, r)
		}
	}

	return w, nil
}

// ForwarderGroup is the Postgres consumer group of the outbox → Redis
// forwarder. It is fixed on purpose: forwarding happens once per message no
// matter how many worker.consumer_groups consume the streams.
const ForwarderGroup = "forwarder"

// addForwarders adds one passthrough handler per distinct consumed topic:
// outbox table (Postgres, ForwarderGroup, starting per start the first time
// the group appears) → Redis Stream of the same name. It returns the topics
// it forwards.
// Per-topic passthrough keeps the existing tables and migrations; Watermill's
// envelope-based forwarder component would need a table of its own.
func addForwarders(
	router *message.Router,
	db *gorm.DB,
	rdb goredis.UniversalClient,
	start eventbus.Start,
	pollInterval time.Duration,
	logger *slog.Logger,
	bcs []struct {
		schema string
		subs   []eventbus.Subscription
	},
) ([]string, error) {
	sub, err := eventbus.NewPostgresSubscriber(db, ForwarderGroup, start, logger, eventbus.WithPollInterval(pollInterval))
	if err != nil {
		return nil, err
	}
	sub = eventbus.Draining(sub, 0) // a forward in progress at SIGTERM completes and is acked
	pub, err := eventbus.NewRedisPublisher(rdb, logger)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var topics []string
	for _, bc := range bcs {
		for _, s := range bc.subs {
			if seen[s.Topic] {
				continue
			}
			seen[s.Topic] = true
			topics = append(topics, s.Topic)
			router.AddHandler("forward:"+s.Topic, s.Topic, sub, s.Topic, pub, message.PassthroughHandler)
		}
	}

	return topics, nil
}

// checkPool is a stub so the tests compile; it refuses nothing yet.
func checkPool(*gorm.DB, []struct {
	schema string
	subs   []eventbus.Subscription
}) error {
	return nil
}

// Run blocks until c is done (SIGTERM in main), then stops taking messages
// and lets the handlers in flight finish and ack (eventbus.Draining, up to
// eventbus.DefaultDrainTimeout) before returning. The lag gauges stop being
// observed when it returns, so a stopped worker does not keep querying the
// stores.
func (w *Worker) Run(c context.Context) error {
	w.logger.Info("worker started", buildinfo.Get().LogAttrs()...)
	defer w.logger.Info("worker stopped")
	defer func() {
		for _, g := range w.gauges {
			_ = g.Unregister()
		}
	}()

	return w.router.Run(c)
}
