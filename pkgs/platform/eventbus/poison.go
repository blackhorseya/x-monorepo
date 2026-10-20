package eventbus

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	wsql "github.com/ThreeDotsLabs/watermill-sql/v4/pkg/sql"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/message/router/middleware"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

// PoisonMessage is one row of a poison table (<bc>.events_poison): the
// message exactly as PoisonQueue parked it, plus what the middleware recorded
// about the failure in its metadata. Offset identifies the row — uuid is the
// message id and repeats when the same message is poisoned twice.
type PoisonMessage struct {
	Offset    int64
	UUID      string
	CreatedAt time.Time
	// Topic the message was consumed from and will be requeued to.
	Topic string
	// Handler that gave up on it (the topic, see Register) and the last error.
	Handler string
	Reason  string
	Payload []byte
	// Metadata is the full metadata, poison keys included.
	Metadata message.Metadata
}

// poisonKeys is what PoisonQueue adds; Requeue strips them so the message
// goes back as it was published (and PoisonQueue sets them again if it
// fails again).
var poisonKeys = []string{
	middleware.ReasonForPoisonedKey,
	middleware.PoisonedTopicKey,
	middleware.PoisonedHandlerKey,
	middleware.PoisonedSubscriberKey,
}

// ListPoison returns up to limit parked messages of poisonTopic, oldest
// first; limit <= 0 means all. Nothing consumes a poison table, so this is
// the whole backlog eventbus.poison.messages counts.
func ListPoison(c context.Context, db *gorm.DB, poisonTopic string, limit int) ([]PoisonMessage, error) {
	q := `SELECT "offset", uuid, created_at, payload, metadata FROM ` + TableName(poisonTopic) + ` ORDER BY "offset"`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := postgres.TxOrDB(c, db).Raw(q).Rows()
	if err != nil {
		return nil, fmt.Errorf("eventbus: list poison %s: %w", poisonTopic, err)
	}
	defer func() { _ = rows.Close() }()

	var out []PoisonMessage
	for rows.Next() {
		m, err := scanPoison(rows)
		if err != nil {
			return nil, fmt.Errorf("eventbus: list poison %s: %w", poisonTopic, err)
		}
		out = append(out, m)
	}

	return out, rows.Err()
}

// Requeue publishes the parked rows back onto the topic each was poisoned
// from (its topic_poisoned metadata) and removes them from the poison table,
// all in one transaction: a row is either back on its topic or still parked,
// never both or neither. The republished message keeps its uuid, payload and
// trace metadata and loses the poison keys. It lands in the topic's outbox
// table like any other publish, so every consumer group behind the topic —
// including the Redis forwarder — picks it up again. offsets empty = every
// row. Returns how many were requeued; rows that no longer exist are not an
// error (someone else handled them).
func Requeue(c context.Context, db *gorm.DB, poisonTopic string, offsets ...int64) (int, error) {
	n := 0
	err := postgres.Transaction(c, db, func(c context.Context, tx *gorm.DB) error {
		rows, err := selectPoison(c, tx, poisonTopic, offsets, true)
		if err != nil {
			return err
		}
		sqlTx, ok := tx.Statement.ConnPool.(*sql.Tx)
		if !ok {
			return errors.New("eventbus: requeue needs a *sql.Tx")
		}
		pub, err := wsql.NewPublisher(wsql.TxFromStdSQL(sqlTx), wsql.PublisherConfig{SchemaAdapter: schema()}, watermill.NopLogger{})
		if err != nil {
			return err
		}
		for _, m := range rows {
			if m.Topic == "" {
				return fmt.Errorf("eventbus: poison row %d has no %s metadata, drop it or requeue by hand", m.Offset, middleware.PoisonedTopicKey)
			}
			msg := message.NewMessage(m.UUID, m.Payload)
			msg.SetContext(c)
			for k, v := range m.Metadata {
				msg.Metadata.Set(k, v)
			}
			for _, k := range poisonKeys {
				delete(msg.Metadata, k)
			}
			if err := pub.Publish(m.Topic, msg); err != nil {
				return fmt.Errorf("eventbus: requeue %d to %s: %w", m.Offset, m.Topic, err)
			}
		}
		n, err = deletePoison(c, tx, poisonTopic, rows)

		return err
	})
	if err != nil {
		return 0, err
	}

	return n, nil
}

// Drop deletes parked rows for good. offsets empty = every row. Returns how
// many were deleted.
func Drop(c context.Context, db *gorm.DB, poisonTopic string, offsets ...int64) (int, error) {
	n := 0
	err := postgres.Transaction(c, db, func(c context.Context, tx *gorm.DB) error {
		rows, err := selectPoison(c, tx, poisonTopic, offsets, true)
		if err != nil {
			return err
		}
		n, err = deletePoison(c, tx, poisonTopic, rows)

		return err
	})
	if err != nil {
		return 0, err
	}

	return n, nil
}

// selectPoison reads the rows (all, or the given offsets), FOR UPDATE when
// lock is set so two operators do not requeue the same row twice.
func selectPoison(c context.Context, tx *gorm.DB, poisonTopic string, offsets []int64, lock bool) ([]PoisonMessage, error) {
	q := `SELECT "offset", uuid, created_at, payload, metadata FROM ` + TableName(poisonTopic)
	var args []any
	if len(offsets) > 0 {
		q += ` WHERE "offset" IN ?`
		args = append(args, offsets)
	}
	q += ` ORDER BY "offset"`
	if lock {
		q += " FOR UPDATE"
	}
	rows, err := tx.Raw(q, args...).Rows()
	if err != nil {
		return nil, fmt.Errorf("eventbus: select poison %s: %w", poisonTopic, err)
	}
	defer func() { _ = rows.Close() }()

	var out []PoisonMessage
	for rows.Next() {
		m, err := scanPoison(rows)
		if err != nil {
			return nil, fmt.Errorf("eventbus: select poison %s: %w", poisonTopic, err)
		}
		out = append(out, m)
	}

	return out, rows.Err()
}

func deletePoison(c context.Context, tx *gorm.DB, poisonTopic string, rows []PoisonMessage) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	offsets := make([]int64, len(rows))
	for i, m := range rows {
		offsets[i] = m.Offset
	}
	res := tx.WithContext(c).Exec(`DELETE FROM `+TableName(poisonTopic)+` WHERE "offset" IN ?`, offsets)
	if res.Error != nil {
		return 0, fmt.Errorf("eventbus: delete poison %s: %w", poisonTopic, res.Error)
	}

	return int(res.RowsAffected), nil
}

func scanPoison(rows *sql.Rows) (PoisonMessage, error) {
	var m PoisonMessage
	var meta []byte
	if err := rows.Scan(&m.Offset, &m.UUID, &m.CreatedAt, &m.Payload, &meta); err != nil {
		return m, err
	}
	m.Metadata = message.Metadata{}
	if len(meta) > 0 {
		if err := json.Unmarshal(meta, &m.Metadata); err != nil {
			return m, fmt.Errorf("row %d metadata: %w", m.Offset, err)
		}
	}
	m.Topic = m.Metadata.Get(middleware.PoisonedTopicKey)
	m.Handler = m.Metadata.Get(middleware.PoisonedHandlerKey)
	m.Reason = strings.TrimSpace(m.Metadata.Get(middleware.ReasonForPoisonedKey))

	return m, nil
}
