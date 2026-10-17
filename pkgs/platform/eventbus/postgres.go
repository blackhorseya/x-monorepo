package eventbus

import (
	"database/sql"
	"fmt"
	"log/slog"

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

// NewPostgresSubscriber reads a topic's table for consumerGroup. Several
// processes with the same group share the work (offsets are per group and
// claimed FOR UPDATE); a different group gets every message again.
func NewPostgresSubscriber(db *gorm.DB, consumerGroup string, l *slog.Logger) (message.Subscriber, error) {
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("eventbus: postgres pool: %w", err)
	}

	return wsql.NewSubscriber(wsql.BeginnerFromStdSQL(sqlDB), wsql.SubscriberConfig{
		ConsumerGroup:    consumerGroup,
		SchemaAdapter:    schema(),
		OffsetsAdapter:   offsets(),
		InitializeSchema: false, // DDL ships in the BC's migrations
	}, NewLogger(l))
}
