package eventbus

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"gorm.io/gorm"
)

// Instruments are the per-attempt consumer metrics Register records inside
// the retry loop (next to access), so every retry is a sample and a parked
// message shows up as `failed`, not as the ack PoisonQueue returns:
//
//	eventbus.consumer.duration{topic, result}   histogram (s), result ok | dropped | failed
//
// Prometheus: eventbus_consumer_duration_seconds_{bucket,count,sum}.
type Instruments struct {
	duration metric.Float64Histogram
}

// NewInstruments creates the consumer histogram on mp. Build one per process
// and share it across every Register call.
func NewInstruments(mp metric.MeterProvider) (*Instruments, error) {
	duration, err := mp.Meter(scopeName).Float64Histogram(
		"eventbus.consumer.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Handler attempt time by result (ok, dropped, failed); a parked message is its last failed attempt."),
		metric.WithExplicitBucketBoundaries(.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10),
	)
	if err != nil {
		return nil, err
	}

	return &Instruments{duration: duration}, nil
}

// middleware records one sample per attempt. It sits with access, innermost.
func (i *Instruments) middleware(h message.HandlerFunc) message.HandlerFunc {
	return func(msg *message.Message) ([]*message.Message, error) {
		start := time.Now()
		out, err := h(msg)

		result := "ok"
		switch {
		case errors.Is(err, ErrDrop):
			result = "dropped"
		case err != nil:
			result = "failed"
		}
		i.duration.Record(msg.Context(), time.Since(start).Seconds(), metric.WithAttributes(
			attribute.String("topic", message.SubscribeTopicFromCtx(msg.Context())),
			attribute.String("result", result),
		))

		return out, err
	}
}

// Cursor names one consumer group's position on one topic, for Lag.
type Cursor struct {
	Topic         string
	ConsumerGroup string
}

// LagOptions lists what a process consumes, so Lag can report how far behind
// each cursor is. Either store may be nil when nothing reads from it.
type LagOptions struct {
	// DB holds the outbox tables; Postgres cursors are measured against it.
	DB *gorm.DB
	// Postgres cursors read the outbox tables directly (handlers in postgres
	// mode, the forwarder in redis mode).
	Postgres []Cursor
	// Redis is the Streams client; Redis cursors are measured against it.
	Redis goredis.UniversalClient
	// RedisStreams cursors read Redis Streams consumer groups.
	RedisStreams []Cursor
	// PoisonTopics are the tables whose row count is the parked backlog.
	PoisonTopics []string
	// OutboxTables are outbox tables (TopicTables) whose size and retention
	// state are reported.
	OutboxTables []string
	// InboxSchemas are the BCs whose `<schema>.inbox` size is reported.
	InboxSchemas []string
}

// Lag registers the backlog gauges, observed on every collection (the OTLP
// periodic reader, or a Prometheus scrape):
//
//	eventbus.consumer.lag{topic, consumer_group, broker}         messages the group has not acked yet
//	eventbus.consumer.oldest_age{topic, consumer_group, broker}  seconds since the oldest of them was written
//	eventbus.poison.messages{topic}                               rows parked on a poison topic (never consumed, so also a running total)
//
// and the retention state of the tables `worker prune` manages:
//
//	eventbus.outbox.rows{table}             rows in the outbox table
//	eventbus.outbox.oldest_age{table}       seconds since the oldest row was written; 0 when empty
//	eventbus.outbox.held{table}             rows some consumer group with a cursor has not acked (prune keeps them, whatever their age)
//	eventbus.outbox.oldest_held_age{table}  seconds since the oldest held row was written; 0 when none
//	eventbus.inbox.rows{schema}             rows in <schema>.inbox
//	eventbus.inbox.oldest_age{schema}       seconds since the oldest inbox row was consumed; 0 when empty
//
// Prometheus: eventbus_consumer_lag, eventbus_consumer_oldest_age_seconds, eventbus_poison_messages,
// eventbus_outbox_rows, eventbus_outbox_oldest_age_seconds, eventbus_outbox_held, eventbus_outbox_oldest_held_age_seconds,
// eventbus_inbox_rows, eventbus_inbox_oldest_age_seconds.
//
// oldest_age past the retention prune runs with means prune is not running or
// cannot delete; oldest_held_age growing for days while every configured
// group's consumer.oldest_age stays low means a group nobody runs any more
// still has a cursor (`worker prune --dry-run` lists every group behind with
// its oldest unread age, whatever the retention; --forget-group).
// held counts against every group in the offsets table, not only the ones
// this process consumes with; a table without any cursor has 0 held.
//
// Postgres lag is exactly what the watermill-sql subscriber could read now:
// rows past the group's (transaction_id, offset) cursor whose transaction is
// below the snapshot's xmin — committed rows behind an older open
// transaction are not counted, because the subscriber cannot see them either.
// A group that has never subscribed is measured from the start of the table.
// Redis lag is XINFO GROUPS lag (not yet delivered) + pending (delivered, not
// acked); age is the older of the first undelivered entry and the oldest
// pending one, both from the entry id's timestamp.
//
// Every replica reports the same numbers: aggregate with max, not sum. The
// callback runs one query per cursor with a 5s budget; unregister the result
// when the process stops consuming.
func Lag(mp metric.MeterProvider, opts LagOptions) (metric.Registration, error) {
	meter := mp.Meter(scopeName)
	lag, err := meter.Int64ObservableGauge("eventbus.consumer.lag",
		metric.WithUnit("{message}"),
		metric.WithDescription("Messages the consumer group has not acknowledged yet."))
	if err != nil {
		return nil, err
	}
	age, err := meter.Float64ObservableGauge("eventbus.consumer.oldest_age",
		metric.WithUnit("s"),
		metric.WithDescription("Seconds since the oldest unacknowledged message was written; 0 when there is none."))
	if err != nil {
		return nil, err
	}
	poison, err := meter.Int64ObservableGauge("eventbus.poison.messages",
		metric.WithUnit("{message}"),
		metric.WithDescription("Messages parked on the poison topic."))
	if err != nil {
		return nil, err
	}

	outboxRows, err := meter.Int64ObservableGauge("eventbus.outbox.rows",
		metric.WithUnit("{row}"),
		metric.WithDescription("Rows in the outbox table."))
	if err != nil {
		return nil, err
	}
	outboxAge, err := meter.Float64ObservableGauge("eventbus.outbox.oldest_age",
		metric.WithUnit("s"),
		metric.WithDescription("Seconds since the oldest outbox row was written; 0 when the table is empty."))
	if err != nil {
		return nil, err
	}
	outboxHeld, err := meter.Int64ObservableGauge("eventbus.outbox.held",
		metric.WithUnit("{row}"),
		metric.WithDescription("Outbox rows a consumer group with a cursor has not acknowledged; prune keeps them whatever their age."))
	if err != nil {
		return nil, err
	}
	outboxHeldAge, err := meter.Float64ObservableGauge("eventbus.outbox.oldest_held_age",
		metric.WithUnit("s"),
		metric.WithDescription("Seconds since the oldest held outbox row was written; 0 when none is held."))
	if err != nil {
		return nil, err
	}
	inboxRows, err := meter.Int64ObservableGauge("eventbus.inbox.rows",
		metric.WithUnit("{row}"),
		metric.WithDescription("Rows in the consumer inbox."))
	if err != nil {
		return nil, err
	}
	inboxAge, err := meter.Float64ObservableGauge("eventbus.inbox.oldest_age",
		metric.WithUnit("s"),
		metric.WithDescription("Seconds since the oldest inbox row was consumed; 0 when the table is empty."))
	if err != nil {
		return nil, err
	}

	var sqlDB *sql.DB
	if opts.DB != nil {
		if sqlDB, err = opts.DB.DB(); err != nil {
			return nil, fmt.Errorf("eventbus: postgres pool: %w", err)
		}
	}
	if sqlDB == nil && (len(opts.Postgres) > 0 || len(opts.PoisonTopics) > 0 || len(opts.OutboxTables) > 0 || len(opts.InboxSchemas) > 0) {
		return nil, errors.New("eventbus: postgres cursors, poison topics, outbox and inbox tables need a database")
	}
	if opts.Redis == nil && len(opts.RedisStreams) > 0 {
		return nil, errors.New("eventbus: redis cursors need a redis client")
	}

	return meter.RegisterCallback(func(c context.Context, o metric.Observer) error {
		c, cancel := context.WithTimeout(c, 5*time.Second)
		defer cancel()
		var errs []error
		for _, cur := range opts.Postgres {
			n, oldest, err := postgresLag(c, sqlDB, cur)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			attrs := metric.WithAttributes(cursorAttrs(cur, "postgres")...)
			o.ObserveInt64(lag, n, attrs)
			o.ObserveFloat64(age, oldest, attrs)
		}
		for _, cur := range opts.RedisStreams {
			n, oldest, err := redisLag(c, opts.Redis, cur)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			attrs := metric.WithAttributes(cursorAttrs(cur, "redis")...)
			o.ObserveInt64(lag, n, attrs)
			o.ObserveFloat64(age, oldest, attrs)
		}
		for _, topic := range opts.PoisonTopics {
			var n int64
			if err := sqlDB.QueryRowContext(c, `SELECT count(*) FROM `+TableName(topic)).Scan(&n); err != nil {
				errs = append(errs, fmt.Errorf("eventbus: poison %s: %w", topic, err))
				continue
			}
			o.ObserveInt64(poison, n, metric.WithAttributes(attribute.String("topic", topic)))
		}
		for _, table := range opts.OutboxTables {
			s, err := outboxState(c, sqlDB, table)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			attrs := metric.WithAttributes(attribute.String("table", table))
			o.ObserveInt64(outboxRows, s.rows, attrs)
			o.ObserveFloat64(outboxAge, s.oldest, attrs)
			o.ObserveInt64(outboxHeld, s.held, attrs)
			o.ObserveFloat64(outboxHeldAge, s.oldestHeld, attrs)
		}
		for _, schema := range opts.InboxSchemas {
			var n int64
			var oldest float64
			err := sqlDB.QueryRowContext(c, `SELECT count(*), coalesce(extract(epoch FROM now() - min(consumed_at)), 0) FROM `+schema+`.inbox`).Scan(&n, &oldest)
			if err != nil {
				errs = append(errs, fmt.Errorf("eventbus: inbox %s: %w", schema, err))
				continue
			}
			attrs := metric.WithAttributes(attribute.String("schema", schema))
			o.ObserveInt64(inboxRows, n, attrs)
			o.ObserveFloat64(inboxAge, oldest, attrs)
		}

		return errors.Join(errs...)
	}, lag, age, poison, outboxRows, outboxAge, outboxHeld, outboxHeldAge, inboxRows, inboxAge)
}

// tableState is one outbox table's size and what retention can do with it.
type tableState struct {
	rows, held         int64
	oldest, oldestHeld float64
}

// outboxState is one scan of the table: every row, and the rows some cursor
// in the offsets table has not passed yet (the unread predicate prune
// negates). Ages are computed in SQL like postgresLag (created_at is
// TIMESTAMP, no zone).
//
// "Some cursor has not passed the row" is the same as "the lowest cursor has
// not": rows and cursors are ordered by the same (transaction_id, offset)
// pair, so the union of the groups' unread ranges is the lowest group's range
// (a NULL offset_acked sorts after the real ones of its transaction and
// leaves only the later transactions unread, as it does per cursor). The
// lowest cursor is picked once from the one-row-per-group offsets table and
// each message row is compared to it; asking the offsets table per message
// row (an EXISTS) was a SubPlan run once per row, ~10x slower at 200k rows.
// No cursor at all holds nothing.
func outboxState(c context.Context, db *sql.DB, table string) (tableState, error) {
	var s tableState
	if err := db.QueryRowContext(c, outboxStateQuery(table)).Scan(&s.rows, &s.oldest, &s.held, &s.oldestHeld); err != nil {
		return s, fmt.Errorf("eventbus: outbox %s: %w", table, err)
	}

	return s, nil
}

func outboxStateQuery(table string) string {
	return `SELECT count(*), coalesce(extract(epoch FROM localtimestamp - min(m.created_at)), 0),
		count(*) FILTER (WHERE held), coalesce(extract(epoch FROM localtimestamp - min(m.created_at) FILTER (WHERE held)), 0)
		FROM (SELECT m.created_at, (m.transaction_id, m."offset") > (o.last_processed_transaction_id, o.offset_acked) AS held
			FROM ` + table + ` m
			LEFT JOIN (SELECT last_processed_transaction_id, offset_acked FROM ` + table + `_offsets
				ORDER BY last_processed_transaction_id, offset_acked LIMIT 1) o ON true) m`
}

func cursorAttrs(cur Cursor, broker string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("topic", cur.Topic),
		attribute.String("consumer_group", cur.ConsumerGroup),
		attribute.String("broker", broker),
	}
}

// postgresLag counts the rows the subscriber predicate would still return for
// the group, and how old the first of them is. Age is computed in SQL:
// created_at is TIMESTAMP (no zone), so comparing it on the Go side would be
// off by the server's zone.
func postgresLag(c context.Context, db *sql.DB, cur Cursor) (int64, float64, error) {
	q := `SELECT count(*), coalesce(extract(epoch FROM localtimestamp - min(m.created_at)), 0)
		FROM ` + TableName(cur.Topic) + ` m
		CROSS JOIN (
			SELECT coalesce(max(offset_acked), 0) AS acked, coalesce(max(last_processed_transaction_id), '0'::xid8) AS tx
			FROM ` + OffsetsTableName(cur.Topic) + ` WHERE consumer_group = $1
		) o
		WHERE m.transaction_id < pg_snapshot_xmin(pg_current_snapshot())
		  AND (m.transaction_id, m."offset") > (o.tx, o.acked)`
	var n int64
	var age float64
	if err := db.QueryRowContext(c, q, cur.ConsumerGroup).Scan(&n, &age); err != nil {
		return 0, 0, fmt.Errorf("eventbus: lag %s/%s: %w", cur.Topic, cur.ConsumerGroup, err)
	}

	return n, age, nil
}

// redisLag is XINFO GROUPS lag + pending for the group; a missing stream or
// group is an empty backlog (the worker creates both at startup).
func redisLag(c context.Context, rdb goredis.UniversalClient, cur Cursor) (int64, float64, error) {
	groups, err := rdb.XInfoGroups(c, cur.Topic).Result()
	if err != nil {
		if strings.Contains(err.Error(), "no such key") {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("eventbus: lag %s/%s: %w", cur.Topic, cur.ConsumerGroup, err)
	}
	var g *goredis.XInfoGroup
	for i := range groups {
		if groups[i].Name == cur.ConsumerGroup {
			g = &groups[i]
			break
		}
	}
	if g == nil {
		return 0, 0, nil
	}
	n := g.Pending
	if g.Lag > 0 { // -1 when Redis cannot tell (after XDEL / XTRIM): report what is known
		n += g.Lag
	}
	if n == 0 {
		return 0, 0, nil
	}

	// Oldest = min(first undelivered entry, oldest pending entry).
	var oldest time.Time
	if g.Lag != 0 {
		next, err := rdb.XRangeN(c, cur.Topic, "("+g.LastDeliveredID, "+", 1).Result()
		if err != nil {
			return 0, 0, fmt.Errorf("eventbus: lag %s/%s: %w", cur.Topic, cur.ConsumerGroup, err)
		}
		if len(next) == 1 {
			oldest = streamIDTime(next[0].ID)
		}
	}
	if g.Pending > 0 {
		pending, err := rdb.XPending(c, cur.Topic, cur.ConsumerGroup).Result()
		if err != nil {
			return 0, 0, fmt.Errorf("eventbus: lag %s/%s: %w", cur.Topic, cur.ConsumerGroup, err)
		}
		if t := streamIDTime(pending.Lower); !t.IsZero() && (oldest.IsZero() || t.Before(oldest)) {
			oldest = t
		}
	}
	if oldest.IsZero() {
		return n, 0, nil
	}
	// Entry ids carry the Redis server's clock: measure against it, not ours.
	now, err := rdb.Time(c).Result()
	if err != nil {
		return 0, 0, fmt.Errorf("eventbus: lag %s/%s: %w", cur.Topic, cur.ConsumerGroup, err)
	}

	return n, max(now.Sub(oldest).Seconds(), 0), nil
}

// streamIDTime is the millisecond timestamp half of a stream entry id.
func streamIDTime(id string) time.Time {
	ms, _, _ := strings.Cut(id, "-")
	v, err := strconv.ParseInt(ms, 10, 64)
	if err != nil {
		return time.Time{}
	}

	return time.UnixMilli(v)
}
