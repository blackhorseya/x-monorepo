package eventbus

import (
	"context"
	"math"
	"slices"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// legacyUnread is what `unread` was before it became a row-value comparison.
// It stays here as the oracle: the two must select the same rows for every
// cursor, including the edges (cursor at zero, mid-transaction, on a
// transaction with no rows, past the end, NULL offset_acked, no cursor row).
const legacyUnread = `(m.transaction_id = o.last_processed_transaction_id AND m."offset" > o.offset_acked)
	OR m.transaction_id > o.last_processed_transaction_id`

const equivTopic = "eqtest.v1.Row"

func TestUnread_RowValueMatchesLegacy(t *testing.T) {
	c := context.Background()
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	db, cleanup, err := platformpg.NewDB(config.Postgres{URL: pgtest.URL(t)}, tel)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}

	msgs, offs := TableName(equivTopic), OffsetsTableName(equivTopic)
	for _, q := range []string{
		"DROP SCHEMA IF EXISTS eqtest CASCADE",
		"CREATE SCHEMA eqtest",
		// The DDL of a BC's events migration, with "offset" given explicitly so
		// the rows below can sit at chosen (transaction_id, offset) positions.
		`CREATE TABLE ` + msgs + ` (
			"offset" BIGINT NOT NULL, uuid VARCHAR(36) NOT NULL, created_at TIMESTAMP NOT NULL,
			payload BYTEA, metadata JSON, transaction_id xid8 NOT NULL,
			PRIMARY KEY (transaction_id, "offset"))`,
		`CREATE TABLE ` + offs + ` (
			consumer_group VARCHAR(255) PRIMARY KEY, offset_acked BIGINT, last_processed_transaction_id xid8 NOT NULL)`,
		// Several transactions, several offsets each; transaction 4 has no rows.
		// Offsets are not monotonic across transactions (the bigserial is global,
		// the transaction id is what orders visibility): tx 2 holds 4..6, tx 3 holds 2.
		`INSERT INTO ` + msgs + ` ("offset", uuid, created_at, transaction_id)
		 SELECT o, 'm' || o || '-' || tx, localtimestamp - make_interval(hours => o::int), tx::text::xid8
		 FROM (VALUES (1,1),(2,1),(3,1),(4,2),(5,2),(6,2),(2,3),(7,3),(8,5),(9,5)) v(o, tx)`,
		`INSERT INTO ` + offs + ` (consumer_group, offset_acked, last_processed_transaction_id) VALUES
			('zero', 0, '0'), ('mid-tx1', 2, '1'), ('end-tx1', 3, '1'), ('mid-tx2', 4, '2'), ('end-tx2', 6, '2'),
			('tx3-low-offset', 1, '3'), ('end-tx3', 7, '3'), ('empty-tx4', 0, '4'), ('end-tx5', 9, '5'),
			('beyond', 100, '9'), ('null-acked', NULL, '2')`,
	} {
		if _, err := sqlDB.ExecContext(c, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	var groups []string
	rows, err := sqlDB.QueryContext(c, "SELECT consumer_group FROM "+offs+" ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			t.Fatal(err)
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()

	// 1. The predicate itself: the same message rows are unread for each cursor.
	unreadSet := func(pred, group string) []string {
		t.Helper()
		r, err := sqlDB.QueryContext(c, `SELECT m.uuid FROM `+msgs+` m JOIN `+offs+` o ON o.consumer_group = $1 AND (`+pred+`) ORDER BY m.uuid`, group)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Close() }()
		var out []string
		for r.Next() {
			var u string
			if err := r.Scan(&u); err != nil {
				t.Fatal(err)
			}
			out = append(out, u)
		}
		if err := r.Err(); err != nil {
			t.Fatal(err)
		}

		return out
	}
	for _, g := range groups {
		if old, now := unreadSet(legacyUnread, g), unreadSet(unread, g); !slices.Equal(old, now) {
			t.Errorf("group %s: unread rows differ: legacy %v, row-value %v", g, old, now)
		}
	}

	// 2. The lag gauge: postgresLag (row-value) against the query it replaced,
	// for every cursor and for a group with no cursor row at all.
	legacyLag := func(group string) (int64, float64) {
		t.Helper()
		q := `SELECT count(*), coalesce(extract(epoch FROM localtimestamp - min(m.created_at)), 0)
			FROM ` + msgs + ` m
			CROSS JOIN (
				SELECT coalesce(max(offset_acked), 0) AS acked, coalesce(max(last_processed_transaction_id), '0'::xid8) AS tx
				FROM ` + offs + ` WHERE consumer_group = $1
			) o
			WHERE m.transaction_id < pg_snapshot_xmin(pg_current_snapshot())
			  AND ((m.transaction_id = o.tx AND m."offset" > o.acked) OR m.transaction_id > o.tx)`
		var n int64
		var age float64
		if err := sqlDB.QueryRowContext(c, q, group).Scan(&n, &age); err != nil {
			t.Fatal(err)
		}

		return n, age
	}
	for _, g := range append(slices.Clone(groups), "no-cursor-row") {
		wantN, wantAge := legacyLag(g)
		gotN, gotAge, err := postgresLag(c, sqlDB, Cursor{Topic: equivTopic, ConsumerGroup: g})
		if err != nil {
			t.Fatalf("group %s: %v", g, err)
		}
		// Ages are read from two statements a few ms apart.
		if gotN != wantN || math.Abs(gotAge-wantAge) > 5 {
			t.Errorf("group %s: lag = (%d, %.1fs), legacy = (%d, %.1fs)", g, gotN, gotAge, wantN, wantAge)
		}
	}
}
