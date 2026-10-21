package eventbus

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

// PruneOptions bound what Prune deletes.
type PruneOptions struct {
	// OlderThan keeps every row younger than this, whatever its state. Required.
	OlderThan time.Duration
	// Unconsumed also prunes topic tables no consumer group has a cursor on
	// (a published-only topic, or a deployment whose worker never ran). Off
	// by default: with no cursor there is no proof anything read the rows.
	Unconsumed bool
	// DryRun counts instead of deleting.
	DryRun bool
	// BatchSize rows per DELETE (default 10000), so a first run on a big
	// table is many short transactions, not one long lock.
	BatchSize int
}

// PruneResult is what Prune did to one table.
type PruneResult struct {
	Table string
	// Rows deleted (or, dry run, that would be).
	Rows int64
	// Skipped says why nothing was touched ("" when the table was pruned).
	Skipped string
}

// TopicTables lists the outbox tables of a schema (`<schema>.events_<topic>`,
// not the offsets tables, not the poison table), from the catalogue rather
// than from code: a published-only topic has no Subscription to find it by.
func TopicTables(c context.Context, db *gorm.DB, schema string) ([]string, error) {
	var names []string
	err := postgres.TxOrDB(c, db).Raw(`SELECT table_name FROM information_schema.tables
		WHERE table_schema = ? AND table_name LIKE 'events\_%' AND table_name NOT LIKE '%\_offsets' AND table_name <> 'events_poison'
		ORDER BY table_name`, schema).Scan(&names).Error
	if err != nil {
		return nil, fmt.Errorf("eventbus: list topic tables of %s: %w", schema, err)
	}
	for i, n := range names {
		names[i] = schema + "." + n
	}

	return names, nil
}

// PruneTopicTable deletes the rows of one outbox table (TopicTables) that are
// older than opts.OlderThan and that every consumer group with a cursor on
// the table has acknowledged — the negation of the "still unread" predicate
// the subscriber and the lag gauge use: a group g has read row m when
// m.transaction_id < g.last_processed_transaction_id, or they are equal and
// m.offset <= g.offset_acked. A table with no cursor at all is skipped
// unless opts.Unconsumed. created_at is TIMESTAMP (no zone), so the age is
// computed against localtimestamp in SQL, like the lag gauge.
func PruneTopicTable(c context.Context, db *gorm.DB, table string, opts PruneOptions) (PruneResult, error) {
	res := PruneResult{Table: table}
	if opts.OlderThan <= 0 {
		return res, fmt.Errorf("eventbus: prune %s: older_than must be positive", table)
	}
	q := postgres.TxOrDB(c, db)
	var groups int64
	if err := q.Raw(`SELECT count(*) FROM ` + table + `_offsets`).Scan(&groups).Error; err != nil {
		return res, fmt.Errorf("eventbus: prune %s: %w", table, err)
	}
	if groups == 0 && !opts.Unconsumed {
		res.Skipped = "no consumer group has a cursor on it (use --unconsumed to prune anyway)"
		return res, nil
	}

	where := `m.created_at < localtimestamp - make_interval(secs => ?)
		AND NOT EXISTS (SELECT 1 FROM ` + table + `_offsets o
			WHERE (m.transaction_id = o.last_processed_transaction_id AND m."offset" > o.offset_acked)
			   OR m.transaction_id > o.last_processed_transaction_id)`
	secs := opts.OlderThan.Seconds()
	if opts.DryRun {
		if err := q.Raw(`SELECT count(*) FROM `+table+` m WHERE `+where, secs).Scan(&res.Rows).Error; err != nil {
			return res, fmt.Errorf("eventbus: prune %s (dry run): %w", table, err)
		}
		return res, nil
	}

	batch := opts.BatchSize
	if batch <= 0 {
		batch = 10000
	}
	for {
		r := q.Exec(`DELETE FROM `+table+` WHERE ctid = ANY(ARRAY(SELECT m.ctid FROM `+table+` m WHERE `+where+` LIMIT ?))`, secs, batch)
		if r.Error != nil {
			return res, fmt.Errorf("eventbus: prune %s: %w", table, r.Error)
		}
		res.Rows += r.RowsAffected
		if r.RowsAffected < int64(batch) {
			return res, nil
		}
	}
}

// PruneInbox deletes a bounded context's inbox rows (`<schema>.inbox`,
// usecasex.Idempotent) older than olderThan, except the ids still parked in
// any poison table: `poison requeue` republishes a message under its original
// id, and the inbox row is what stops a BC that already consumed it from
// doing so again. Every BC's poison table counts, because BC A's inbox
// protects against BC B's requeue.
func PruneInbox(c context.Context, db *gorm.DB, schema string, olderThan time.Duration, poisonSchemas []string, dryRun bool) (PruneResult, error) {
	res := PruneResult{Table: schema + ".inbox"}
	if olderThan <= 0 {
		return res, fmt.Errorf("eventbus: prune %s: older_than must be positive", res.Table)
	}
	parked := make([]string, 0, len(poisonSchemas))
	for _, s := range poisonSchemas {
		parked = append(parked, `SELECT uuid FROM `+TableName(PoisonTopic(s)))
	}
	where := `consumed_at < now() - make_interval(secs => ?)`
	if len(parked) > 0 {
		where += ` AND message_id NOT IN (` + strings.Join(parked, " UNION ") + `)`
	}
	q := postgres.TxOrDB(c, db)
	if dryRun {
		if err := q.Raw(`SELECT count(*) FROM `+res.Table+` WHERE `+where, olderThan.Seconds()).Scan(&res.Rows).Error; err != nil {
			return res, fmt.Errorf("eventbus: prune %s (dry run): %w", res.Table, err)
		}
		return res, nil
	}
	r := q.Exec(`DELETE FROM `+res.Table+` WHERE `+where, olderThan.Seconds())
	if r.Error != nil {
		return res, fmt.Errorf("eventbus: prune %s: %w", res.Table, r.Error)
	}
	res.Rows = r.RowsAffected

	return res, nil
}
