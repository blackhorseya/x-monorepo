package eventbus

import (
	"context"
	"database/sql"
	"math"
	"strings"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// legacyOutboxState is outboxState's query from when "held" was an EXISTS
// subquery run once per message row. It stays here as the oracle: the rewrite
// must report the same rows / held / ages for every set of cursors.
func legacyOutboxState(table string) string {
	return `SELECT count(*), coalesce(extract(epoch FROM localtimestamp - min(m.created_at)), 0),
		count(*) FILTER (WHERE held), coalesce(extract(epoch FROM localtimestamp - min(m.created_at) FILTER (WHERE held)), 0)
		FROM (SELECT m.created_at, EXISTS (SELECT 1 FROM ` + table + `_offsets o WHERE ` + unread + `) AS held FROM ` + table + ` m) m`
}

const stateTopic = "ostest.v1.Row"

// stateDB is an outbox table with the rows of TestUnread_RowValueMatchesLegacy
// (ten rows, five transactions, transaction 4 empty, offsets not monotonic
// across transactions) and an empty offsets table.
func stateDB(t *testing.T) (context.Context, *sql.DB) {
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
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	msgs, offs := TableName(stateTopic), OffsetsTableName(stateTopic)
	for _, q := range []string{
		"DROP SCHEMA IF EXISTS ostest CASCADE",
		"CREATE SCHEMA ostest",
		`CREATE TABLE ` + msgs + ` (
			"offset" BIGINT NOT NULL, uuid VARCHAR(36) NOT NULL, created_at TIMESTAMP NOT NULL,
			payload BYTEA, metadata JSON, transaction_id xid8 NOT NULL,
			PRIMARY KEY (transaction_id, "offset"))`,
		`CREATE TABLE ` + offs + ` (
			consumer_group VARCHAR(255) PRIMARY KEY, offset_acked BIGINT, last_processed_transaction_id xid8 NOT NULL)`,
		`INSERT INTO ` + msgs + ` ("offset", uuid, created_at, transaction_id)
		 SELECT o, 'm' || o || '-' || tx, localtimestamp - make_interval(hours => o::int), tx::text::xid8
		 FROM (VALUES (1,1),(2,1),(3,1),(4,2),(5,2),(6,2),(2,3),(7,3),(8,5),(9,5)) v(o, tx)`,
	} {
		if _, err := sqlDB.ExecContext(c, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	return c, sqlDB
}

func TestOutboxState_MatchesLegacyExists(t *testing.T) {
	c, sqlDB := stateDB(t)
	msgs, offs := TableName(stateTopic), OffsetsTableName(stateTopic)

	for name, cursors := range map[string]string{
		"no cursor":                       ``,
		"one at zero":                     `('a', 0, '0')`,
		"mid transaction":                 `('a', 2, '1')`,
		"end of transaction":              `('a', 6, '2')`,
		"transaction 3, low offset":       `('a', 1, '3')`,
		"empty transaction":               `('a', 0, '4')`,
		"beyond the end":                  `('a', 100, '9')`,
		"NULL offset_acked":               `('a', NULL, '2')`,
		"several, lagging one decides":    `('a', 100, '9'), ('b', 3, '1'), ('c', 7, '3')`,
		"NULL below a real one":           `('a', NULL, '2'), ('b', 5, '2'), ('c', 9, '5')`,
		"NULL above a real one":           `('a', NULL, '3'), ('b', 5, '2')`,
		"same transaction, NULL and real": `('a', NULL, '2'), ('b', 4, '2')`,
	} {
		if _, err := sqlDB.ExecContext(c, "DELETE FROM "+offs); err != nil {
			t.Fatal(err)
		}
		if cursors != "" {
			if _, err := sqlDB.ExecContext(c, "INSERT INTO "+offs+" (consumer_group, offset_acked, last_processed_transaction_id) VALUES "+cursors); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		var want, got tableState
		if err := sqlDB.QueryRowContext(c, legacyOutboxState(msgs)).Scan(&want.rows, &want.oldest, &want.held, &want.oldestHeld); err != nil {
			t.Fatalf("%s: legacy: %v", name, err)
		}
		got, err := outboxState(c, sqlDB, msgs)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// Ages are read from two statements a few ms apart.
		if got.rows != want.rows || got.held != want.held || math.Abs(got.oldest-want.oldest) > 5 || math.Abs(got.oldestHeld-want.oldestHeld) > 5 {
			t.Errorf("%s: outboxState = %+v, legacy EXISTS = %+v", name, got, want)
		}
	}
}

// The gauge runs on every scrape over the whole table, so "held" must not
// cost a probe of the offsets table per message row: that SubPlan took ~240ms
// at 200k rows, against ~24ms for comparing each row to the lowest cursor.
func TestOutboxState_NoPerRowSubplan(t *testing.T) {
	c, sqlDB := stateDB(t)
	rows, err := sqlDB.QueryContext(c, "EXPLAIN "+outboxStateQuery(TableName(stateTopic)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, l)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if p := strings.Join(plan, "\n"); strings.Contains(p, "SubPlan") {
		t.Errorf("outboxState probes the offsets table once per row:\n%s", p)
	}
}
