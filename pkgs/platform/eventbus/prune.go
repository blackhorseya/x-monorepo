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
	// table is many short statements, not one long one. Each commits on its
	// own only when the context carries no transaction (the CLI and the admin
	// RPC carry none); inside one they all share its lifetime and locks.
	BatchSize int
	// ForgetGroups are consumer groups whose cursors are deleted from the
	// table's offsets table first, so they no longer hold rows back: groups
	// that will never run again (an E2E run against this database, a
	// worker.consumer_group that was renamed, a BC group after the broker
	// moved to redis). If such a group does come back it starts over as a new
	// group (worker.start_from). A DryRun deletes none of the cursors but
	// reports them as Forgotten and counts rows and Blockers as if they were
	// already gone.
	ForgetGroups []string
}

// PruneResult is what Prune did to one table.
type PruneResult struct {
	Table string
	// Rows deleted (or, dry run, that would be).
	Rows int64
	// Skipped says why nothing was touched ("" when the table was pruned).
	Skipped string
	// Blockers are the consumer groups with rows they have not read on this
	// table, oldest unread first, whatever the rows' age — the same "held"
	// the outbox gauges report. The ones whose OldestUnread is past
	// OlderThan are what keeps prunable rows from going; a group that will
	// never catch up belongs in ForgetGroups.
	Blockers []Blocker
	// Forgotten are the ForgetGroups that had a cursor on this table.
	Forgotten []string
}

// unread is the subscriber's / lag gauge's "row m is still to read for the
// cursor row o" predicate, shared by prune (its negation is "prunable") and
// the outbox gauges (its existence over all groups is "held back").
//
// It is a row-value comparison, not (tx = o.tx AND offset > acked) OR tx > o.tx:
// the two are the same ordering, but only this form lets the planner range-scan
// the (transaction_id, offset) primary key instead of reading the whole table.
// A NULL offset_acked behaves as before (equal transaction ids → not unread).
const unread = `(m.transaction_id, m."offset") > (o.last_processed_transaction_id, o.offset_acked)`

// Blocker is a consumer group behind on a table: Unread rows it has not
// read, the oldest of them OldestUnread old. Past PruneOptions.OlderThan the
// group holds prunable rows back; younger, it is merely behind (a live group
// between two polls shows up like that).
type Blocker struct {
	Group        string
	Unread       int64
	OldestUnread time.Duration
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
	secs := opts.OlderThan.Seconds()
	if len(opts.ForgetGroups) > 0 {
		if err := q.Raw(`SELECT consumer_group FROM `+table+`_offsets WHERE consumer_group IN ? ORDER BY consumer_group`, opts.ForgetGroups).Scan(&res.Forgotten).Error; err != nil {
			return res, fmt.Errorf("eventbus: prune %s: %w", table, err)
		}
		if !opts.DryRun && len(res.Forgotten) > 0 {
			if err := q.Exec(`DELETE FROM `+table+`_offsets WHERE consumer_group IN ?`, res.Forgotten).Error; err != nil {
				return res, fmt.Errorf("eventbus: prune %s: forget groups: %w", table, err)
			}
		}
	}
	var groups int64
	if err := q.Raw(`SELECT count(*) FROM ` + table + `_offsets`).Scan(&groups).Error; err != nil {
		return res, fmt.Errorf("eventbus: prune %s: %w", table, err)
	}
	if opts.DryRun {
		groups -= int64(len(res.Forgotten))
	}
	if groups == 0 && !opts.Unconsumed {
		res.Skipped = "no consumer group has a cursor on it (unconsumed prunes it anyway)"
		return res, nil
	}

	offsets := table + `_offsets o`
	if opts.DryRun && len(res.Forgotten) > 0 {
		offsets = `(SELECT * FROM ` + table + `_offsets WHERE consumer_group NOT IN ?) o`
	}
	where := `m.created_at < localtimestamp - make_interval(secs => ?)
		AND NOT EXISTS (SELECT 1 FROM ` + offsets + ` WHERE ` + unread + `)`
	args := []any{secs}
	if opts.DryRun && len(res.Forgotten) > 0 {
		args = append(args, res.Forgotten)
	}
	// Who is behind, by how much and since when — no age filter, so a stale
	// group shows up before its rows reach the retention (the held gauge and
	// its alert see it that early too), whatever happens next.
	blockQ := `SELECT o.consumer_group AS "group", count(*) AS unread,
		extract(epoch FROM localtimestamp - min(m.created_at)) AS oldest_secs
		FROM ` + table + ` m JOIN ` + offsets + ` ON (` + unread + `) GROUP BY 1 ORDER BY 3 DESC, 1`
	var blockArgs []any
	if opts.DryRun && len(res.Forgotten) > 0 {
		blockArgs = []any{res.Forgotten}
	}
	var blockers []struct {
		Group      string
		Unread     int64
		OldestSecs float64
	}
	if err := q.Raw(blockQ, blockArgs...).Scan(&blockers).Error; err != nil {
		return res, fmt.Errorf("eventbus: prune %s: blockers: %w", table, err)
	}
	for _, b := range blockers {
		res.Blockers = append(res.Blockers, Blocker{Group: b.Group, Unread: b.Unread, OldestUnread: time.Duration(b.OldestSecs * float64(time.Second))})
	}
	if opts.DryRun {
		if err := q.Raw(`SELECT count(*) FROM `+table+` m WHERE `+where, args...).Scan(&res.Rows).Error; err != nil {
			return res, fmt.Errorf("eventbus: prune %s (dry run): %w", table, err)
		}
		return res, nil
	}

	batch := opts.BatchSize
	if batch <= 0 {
		batch = 10000
	}
	for {
		r := q.Exec(`DELETE FROM `+table+` WHERE ctid = ANY(ARRAY(SELECT m.ctid FROM `+table+` m WHERE `+where+` LIMIT ?))`, append(args, batch)...)
		if r.Error != nil {
			return res, fmt.Errorf("eventbus: prune %s: %w", table, r.Error)
		}
		res.Rows += r.RowsAffected
		if r.RowsAffected < int64(batch) {
			return res, nil
		}
	}
}

// PruneSchema prunes one bounded context: every topic table of the schema
// (TopicTables, in name order) with opts, then its inbox with opts.OlderThan
// and opts.DryRun. poisonSchemas are ALL the BCs whose poison tables may
// still park a message id (PruneInbox), not just this one. The CLI and the
// admin RPC both run this; results are in table order, the inbox last.
func PruneSchema(c context.Context, db *gorm.DB, schema string, opts PruneOptions, poisonSchemas []string) ([]PruneResult, error) {
	tables, err := TopicTables(c, db, schema)
	if err != nil {
		return nil, err
	}
	results := make([]PruneResult, 0, len(tables)+1)
	for _, tbl := range tables {
		res, err := PruneTopicTable(c, db, tbl, opts)
		if err != nil {
			return results, err
		}
		results = append(results, res)
	}
	res, err := PruneInbox(c, db, schema, opts.OlderThan, poisonSchemas, opts.DryRun)
	if err != nil {
		return results, err
	}

	return append(results, res), nil
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
