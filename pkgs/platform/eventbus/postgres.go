package eventbus

import (
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	wsql "github.com/ThreeDotsLabs/watermill-sql/v4/pkg/sql"
	"github.com/ThreeDotsLabs/watermill/message"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

// schema is the watermill-sql Postgres schema adapter with this repo's table
// naming (TableName) and proto bytes as payload. The DDL it would run with
// InitializeSchema is what each BC's migration contains; see
// pkgs/ordering/infra/postgres/migrations/0003_events.up.sql.
func schema() wsql.DefaultPostgreSQLSchema {
	return wsql.DefaultPostgreSQLSchema{
		GenerateMessagesTableName: TableName,
		GeneratePayloadType:       func(string) string { return "BYTEA" },
	}
}

func offsets() wsql.DefaultPostgreSQLOffsetsAdapter {
	return wsql.DefaultPostgreSQLOffsetsAdapter{GenerateMessagesOffsetsTableName: OffsetsTableName}
}

// Start is where a consumer group with no cursor yet begins. A group that
// already has a cursor is never moved.
type Start int

const (
	// FromBeginning replays everything the table / stream still holds.
	FromBeginning Start = iota
	// FromLatest starts after what the subscriber could already see when the
	// group was created.
	FromLatest
)

// latestOffsets seeds a brand-new consumer group at the newest visible
// message instead of the library's (0, '0'). The seed pair must satisfy the
// same predicate the subscriber reads with: it takes the row with the highest
// (transaction_id, offset) among rows whose transaction_id is below the
// current snapshot's xmin, i.e. exactly what a subscriber created at this
// moment would already have been able to consume. Anything committed later,
// or still in flight now, has a higher transaction_id and is delivered.
// ON CONFLICT DO NOTHING keeps an existing cursor untouched.
type latestOffsets struct {
	wsql.DefaultPostgreSQLOffsetsAdapter
}

func (a latestOffsets) BeforeSubscribingQueries(params wsql.BeforeSubscribingQueriesParams) ([]wsql.Query, error) {
	return []wsql.Query{{
		Query: `INSERT INTO ` + a.MessagesOffsetsTable(params.Topic) + ` (consumer_group, offset_acked, last_processed_transaction_id)
			SELECT $1, coalesce(last."offset", 0), coalesce(last.transaction_id, '0'::xid8)
			FROM (SELECT NULL) AS one
			LEFT JOIN LATERAL (
				SELECT "offset", transaction_id FROM ` + TableName(params.Topic) + `
				WHERE transaction_id < pg_snapshot_xmin(pg_current_snapshot())
				ORDER BY transaction_id DESC, "offset" DESC
				LIMIT 1
			) AS last ON true
			ON CONFLICT DO NOTHING;`,
		Args: []any{params.ConsumerGroup},
	}}, nil
}

// PostgresPublisher is the outbox side: Publish writes the message rows
// through the transaction carried by the message context
// (platform/postgres.WithTx, i.e. whatever the use case's Transactor opened),
// so an event is committed exactly when the state change is. Without a
// transaction in the context it writes autocommit.
type PostgresPublisher struct {
	db     *gorm.DB
	sqlDB  *sql.DB
	logger watermill.LoggerAdapter
}

var _ message.Publisher = (*PostgresPublisher)(nil)

func NewPostgresPublisher(db *gorm.DB, l *slog.Logger) (*PostgresPublisher, error) {
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("eventbus: postgres pool: %w", err)
	}

	return &PostgresPublisher{db: db, sqlDB: sqlDB, logger: NewLogger(l)}, nil
}

func (p *PostgresPublisher) Publish(topic string, msgs ...*message.Message) error {
	if len(msgs) == 0 {
		return nil
	}

	// All messages of one Publish share the first message's transaction: the
	// events adapter publishes one aggregate's events together.
	exec := wsql.ContextExecutor(wsql.BeginnerFromStdSQL(p.sqlDB))
	if tx, ok := postgres.TxOrDB(msgs[0].Context(), p.db).Statement.ConnPool.(*sql.Tx); ok {
		exec = wsql.TxFromStdSQL(tx)
	}

	pub, err := wsql.NewPublisher(exec, wsql.PublisherConfig{SchemaAdapter: schema()}, p.logger)
	if err != nil {
		return fmt.Errorf("eventbus: publisher: %w", err)
	}

	return pub.Publish(topic, msgs...)
}

// Close is a no-op: the pool belongs to platform/postgres.NewDB.
func (p *PostgresPublisher) Close() error { return nil }

// SubscriberOption tunes NewPostgresSubscriber.
type SubscriberOption func(*subscriberOptions)

type subscriberOptions struct {
	pollInterval time.Duration
}

// WithPollInterval is how long the subscriber waits before it asks the outbox
// table again after a poll that found nothing (watermill-sql PollInterval).
// It is the floor of the consumer-side latency of an event that arrives while
// the subscriber is idle, and every subscriber (one per handler topic and BC,
// per worker replica) issues one query per interval. 0 keeps the library's 1s.
// The resend wait after a nack and the wait after a failed query keep the
// library's 1s: handler failures are retried in-process by Register's Retry
// middleware long before a nack, and a failing database should not be polled
// faster.
func WithPollInterval(d time.Duration) SubscriberOption {
	return func(o *subscriberOptions) { o.pollInterval = d }
}

// subscriberConfig is the watermill-sql config NewPostgresSubscriber builds,
// split out so a test can read what was passed down.
func subscriberConfig(consumerGroup string, offsetsAdapter wsql.OffsetsAdapter, opts ...SubscriberOption) wsql.SubscriberConfig {
	var o subscriberOptions
	for _, opt := range opts {
		opt(&o)
	}

	return wsql.SubscriberConfig{
		ConsumerGroup:    consumerGroup,
		SchemaAdapter:    schema(),
		OffsetsAdapter:   offsetsAdapter,
		PollInterval:     o.pollInterval,
		InitializeSchema: false, // DDL ships in the BC's migrations
	}
}

// NewPostgresSubscriber reads a topic's table for consumerGroup. Several
// processes with the same group share the work (offsets are per group and
// claimed FOR UPDATE); a different group gets every message again from
// start (FromBeginning) or only what arrives after it was created (FromLatest).
func NewPostgresSubscriber(db *gorm.DB, consumerGroup string, start Start, l *slog.Logger, opts ...SubscriberOption) (message.Subscriber, error) {
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("eventbus: postgres pool: %w", err)
	}
	var offsetsAdapter wsql.OffsetsAdapter = offsets()
	if start == FromLatest {
		offsetsAdapter = latestOffsets{offsets()}
	}

	return wsql.NewSubscriber(wsql.BeginnerFromStdSQL(sqlDB), subscriberConfig(consumerGroup, offsetsAdapter, opts...), NewLogger(l))
}
