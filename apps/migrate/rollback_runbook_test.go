package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// A single-BC rollback runbook (docs/operations.md) must move its own BC back one version and
// leave every other BC — version, tables, columns, rows — alone. Each case below runs the entry
// the runbook gives (the ```sql block after its marker, or the legacy `migrate down` statement),
// never postgres.Down(<bc>): the point is the thing an operator copies. `migrate down` itself
// steps every BC back, which is why a runbook must not point at it for one BC.
//
// Adding or changing a BC rollback runbook: add a rollbackCase for every state the runbook says
// it applies to (clean, dirty, ...). docs/workflow.md "Review contracts".

// rollbackCase is one state a runbook's rollback entry is claimed to work from.
type rollbackCase struct {
	name       string
	runbook    string // file the entry is read from
	marker     string // the section marker; the first ```sql block after it is the entry
	legacyDown string // runbook text meaning "run `task migrate -- down`" (an entry that is not SQL)
	bc         int    // index in migrations
	latest     uint   // the BC's version before the rollback
	// prepare brings the BC to the starting state beyond "every migration applied".
	prepare func(t *testing.T, e *rollbackEnv)
	// afterRollback checks the target BC's own state once it is back one version.
	afterRollback func(t *testing.T, e *rollbackEnv)
	// reapply gets the BC ready for, runs and checks the next migrate up (removes a fault, then up).
	reapply func(t *testing.T, e *rollbackEnv)
}

type rollbackEnv struct {
	c    context.Context
	url  string
	cfg  config.Postgres
	conn *pgx.Conn
}

func (e *rollbackEnv) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.conn.Exec(e.c, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func (e *rollbackEnv) count(t *testing.T, q string) int {
	t.Helper()
	var n int
	if err := e.conn.QueryRow(e.c, q).Scan(&n); err != nil {
		t.Fatal(err)
	}

	return n
}

func (e *rollbackEnv) version(t *testing.T, bc int) string {
	t.Helper()
	v, dirty, err := postgres.Version(e.c, e.cfg, migrations[bc])
	if err != nil {
		t.Fatal(err)
	}

	return fmt.Sprintf("%d dirty=%v", v, dirty)
}

// snapshot describes every base table of the schemas — name, columns, and a hash of the rows
// (the version table included) — so any change to a BC shows up as a different string.
func (e *rollbackEnv) snapshot(t *testing.T, schemas []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, schema := range schemas {
		rows, err := e.conn.Query(e.c, `SELECT table_name FROM information_schema.tables WHERE table_schema = $1 AND table_type = 'BASE TABLE' ORDER BY 1`, schema)
		if err != nil {
			t.Fatal(err)
		}
		var tables []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			tables = append(tables, name)
		}
		rows.Close()

		var parts []string
		for _, table := range tables {
			var cols, data string
			if err := e.conn.QueryRow(e.c, `SELECT coalesce(string_agg(column_name || ':' || data_type, ',' ORDER BY ordinal_position), '') FROM information_schema.columns WHERE table_schema = $1 AND table_name = $2`, schema, table).Scan(&cols); err != nil {
				t.Fatal(err)
			}
			q := fmt.Sprintf(`SELECT count(*)::text || ':' || coalesce(md5(string_agg(r::text, '|' ORDER BY r::text)), '') FROM %s r`, pgx.Identifier{schema, table}.Sanitize())
			if err := e.conn.QueryRow(e.c, q).Scan(&data); err != nil {
				t.Fatal(err)
			}
			parts = append(parts, table+" ["+cols+"] "+data)
		}
		out[schema] = strings.Join(parts, "\n")
	}

	return out
}

// bcSchemas are the schemas whose tables the snapshots cover.
var bcSchemas = []string{"catalog", "identity", "inventory", "ordering", "payment"}

// dataTables lists every base table of the BC schemas except the version tables and the
// catalog.currencies list migration 0001 seeds.
func (e *rollbackEnv) dataTables(t *testing.T) []string {
	t.Helper()
	rows, err := e.conn.Query(e.c, `SELECT table_schema || '.' || table_name FROM information_schema.tables
		WHERE table_schema = ANY($1) AND table_type = 'BASE TABLE' AND table_name <> 'schema_migrations' AND table_schema || '.' || table_name <> 'catalog.currencies'
		ORDER BY 1`, bcSchemas)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}

	return out
}

// seedEveryTable empties the data tables and puts one row in each, so no table the snapshot
// compares is empty. events_* / *_offsets tables get a generic row; the rest are listed.
func (e *rollbackEnv) seedEveryTable(t *testing.T) {
	t.Helper()
	tables := e.dataTables(t)
	e.exec(t, `TRUNCATE `+strings.Join(tables, ", ")+` CASCADE`)

	e.exec(t, `INSERT INTO catalog.products (sku, name, price_amount, price_currency, active, updated_at) VALUES ('ROLLBACK-SENTINEL', 'sentinel', 100, 'USD', true, now())`)

	e.exec(t, `INSERT INTO identity.accounts (id, name, status, created_at, updated_at) VALUES ('acct-sentinel', 'sentinel', 'ACTIVE', now(), now())`)
	e.exec(t, `INSERT INTO identity.api_keys (id, account_id, name, prefix, hash, status, created_at) VALUES ('key-sentinel', 'acct-sentinel', 'sentinel', 'pfx', decode(repeat('00', 32), 'hex'), 'ACTIVE', now())`)
	e.exec(t, `INSERT INTO identity.audit_events (actor, action, target_account_id, occurred_at) VALUES ('sentinel', 'sentinel', 'acct-sentinel', now())`)
	e.exec(t, `INSERT INTO identity.external_identities (issuer, subject, account_id, created_at) VALUES ('https://sentinel.example', 'sub-sentinel', 'acct-sentinel', now())`)
	e.exec(t, `INSERT INTO identity.invites (id, email, account_id, created_at, expires_at) VALUES ('invite-sentinel', 'sentinel@example.com', 'acct-sentinel', now(), now() + interval '1 day')`)

	e.exec(t, `INSERT INTO inventory.stocks (sku, on_hand, reserved, version, updated_at) VALUES ('ROLLBACK-STOCK', 3, 0, 1, now())`)
	e.exec(t, `INSERT INTO inventory.reservations (order_id, status, created_at, updated_at) VALUES ('rollback-order', 'RESERVED', now(), now())`)
	e.exec(t, `INSERT INTO inventory.reservation_lines (order_id, position, sku, quantity) VALUES ('rollback-order', 0, 'ROLLBACK-STOCK', 1)`)
	e.exec(t, `INSERT INTO inventory.inbox (message_id) VALUES ('sentinel')`)

	e.exec(t, `INSERT INTO ordering.orders (id, customer_id, status, created_at, updated_at) VALUES ('rollback-order', 'rollback-customer', 'PENDING', now(), now())`)
	e.exec(t, `INSERT INTO ordering.order_items (order_id, position, sku, quantity, unit_price_minor, currency) VALUES ('rollback-order', 0, 'ROLLBACK-STOCK', 1, 100, 'USD')`)
	e.exec(t, `INSERT INTO ordering.inbox (message_id) VALUES ('sentinel')`)
	e.exec(t, `INSERT INTO ordering.order_history (order_id, seq, event, from_status, to_status, occurred_at, actor_kind, actor_subject, cause) VALUES ('rollback-order', 1, 'CREATED', NULL, 'PENDING', now(), 'SYSTEM', '', 'RPC_REQUEST')`)

	e.exec(t, `INSERT INTO payment.payments (id, order_id, customer_id, amount, currency, status, order_outcome, expires_at, version, updated_at) VALUES ('payment-sentinel', 'rollback-order', 'rollback-customer', 100, 'USD', 'PENDING', 'NONE', now() + interval '1 hour', 1, now())`)
	e.exec(t, `INSERT INTO payment.inbox (message_id) VALUES ('sentinel')`)
	e.exec(t, `INSERT INTO payment.order_outcomes (order_id, outcome, recorded_at) VALUES ('rollback-order', 'CANCELLED', now())`)
	e.exec(t, `INSERT INTO payment.webhook_events (provider, provider_event_id, event_type, provider_ref, body, received_at) VALUES ('mock', 'sentinel', 'payment.captured', 'ref-sentinel', '\x7b7d', now())`)
	e.exec(t, `INSERT INTO payment.mock_checkouts (ref, payment_id, amount, currency, url, expires_at, paid, paid_amount, paid_currency) VALUES ('mock-ref-sentinel', 'payment-sentinel', 100, 'USD', 'https://mock-pay.invalid/checkout/sentinel', now() + interval '1 hour', true, 100, 'USD')`)
	e.exec(t, `INSERT INTO payment.mock_refunds (ref, idempotency_key) VALUES ('mock-ref-sentinel', 'sentinel')`)
	e.exec(t, `INSERT INTO payment.mock_faults (op) VALUES ('get_payment')`)

	for _, table := range tables {
		switch {
		case strings.Contains(table, ".events_") && strings.HasSuffix(table, "_offsets"):
			e.exec(t, `INSERT INTO `+table+` (consumer_group, offset_acked, last_processed_transaction_id) VALUES ('sentinel', 0, pg_current_xact_id())`)
		case strings.Contains(table, ".events_"):
			e.exec(t, `INSERT INTO `+table+` (uuid, payload, metadata, transaction_id) VALUES ('sentinel', '\x00'::bytea, '{}', pg_current_xact_id())`)
		}
	}
}

// requireEveryTableSeeded fails when any data table is empty.
func (e *rollbackEnv) requireEveryTableSeeded(t *testing.T) {
	t.Helper()
	var empty []string
	for _, table := range e.dataTables(t) {
		if e.count(t, `SELECT count(*) FROM `+table) == 0 {
			empty = append(empty, table)
		}
	}
	if len(empty) > 0 {
		t.Fatalf("tables with no sentinel row (the snapshot would compare nothing): %s", strings.Join(empty, ", "))
	}
}

// runbookEntry runs the rollback entry the runbook file gives.
func (e *rollbackEnv) runbookEntry(t *testing.T, tc rollbackCase) {
	t.Helper()
	b, err := os.ReadFile(tc.runbook)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	_, section, hasMarker := strings.Cut(doc, tc.marker)
	switch {
	case hasMarker:
		m := regexp.MustCompile("(?s)```sql\n(.*?)```").FindStringSubmatch(section)
		if m == nil {
			t.Fatalf("%s: no ```sql block after %q", tc.runbook, tc.marker)
		}
		e.exec(t, m[1])
	case tc.legacyDown != "" && strings.Contains(doc, tc.legacyDown):
		t.Setenv("APP_POSTGRES_URL", e.url)
		if err := run([]string{"down"}); err != nil {
			t.Fatalf("migrate down: %v", err)
		}
	default:
		t.Fatalf("%s has neither %q nor %q", tc.runbook, tc.marker, tc.legacyDown)
	}
}

func runRollbackCase(t *testing.T, tc rollbackCase) {
	t.Helper()
	url := pgtest.URL(t)
	e := &rollbackEnv{c: context.Background(), url: url, cfg: config.Postgres{URL: url}}

	for _, m := range migrations { // every BC at its latest
		if err := postgres.Up(e.c, e.cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	conn := pgtest.Conn(t, url)
	e.conn = conn
	t.Cleanup(func() { // leave every BC applied and clean for the next test, whatever the runbook SQL left behind
		ctx := context.Background()
		// First: a runbook that failed halfway leaves its transaction (and locks) open on it
		_ = conn.Close(ctx)
		fresh := pgtest.Conn(t, url)                                                                                            // closed when this cleanup returns, after the restore below
		if _, err := fresh.Exec(ctx, `DELETE FROM inventory.stocks WHERE octet_length(sku) NOT BETWEEN 1 AND 64`); err != nil { // the dirty case's over-long row would fail 0009 again
			t.Errorf("restore: delete over-long skus: %v", err)
		}
		for _, m := range migrations {
			if _, err := fresh.Exec(ctx, fmt.Sprintf(`UPDATE %s.schema_migrations SET version = version - 1, dirty = false WHERE dirty`, pgx.Identifier{m.Schema}.Sanitize())); err != nil {
				t.Errorf("restore %s: clear dirty flag: %v", m.Schema, err)
			}
			if err := postgres.Up(ctx, e.cfg, m); err != nil {
				t.Errorf("restore %s: %v", m.Schema, err)
			}
		}
	})

	e.seedEveryTable(t)

	if tc.prepare != nil {
		tc.prepare(t, e)
	}
	e.requireEveryTableSeeded(t) // an empty table would compare equal whatever the runbook did to it

	target := migrations[tc.bc].Schema
	var others []string
	for _, m := range migrations {
		if m.Schema != target {
			others = append(others, m.Schema)
		}
	}
	before := e.snapshot(t, others)
	otherVersions := map[string]string{}
	for i, m := range migrations {
		if i != tc.bc {
			otherVersions[m.Schema] = e.version(t, i)
		}
	}

	e.runbookEntry(t, tc)

	if got, want := e.version(t, tc.bc), fmt.Sprintf("%d dirty=false", tc.latest-1); got != want {
		t.Errorf("%s: %s, want %s", target, got, want)
	}
	for i, m := range migrations {
		if i != tc.bc {
			if got := e.version(t, i); got != otherVersions[m.Schema] {
				t.Errorf("%s: %s, want %s", m.Schema, got, otherVersions[m.Schema])
			}
		}
	}
	after := e.snapshot(t, others)
	for _, schema := range others {
		if after[schema] != before[schema] {
			t.Errorf("schema %s changed by the %s rollback:\nbefore:\n%s\nafter:\n%s", schema, target, before[schema], after[schema])
		}
	}
	if tc.afterRollback != nil {
		tc.afterRollback(t, e)
	}
	if tc.reapply != nil {
		tc.reapply(t, e)
	}
}

const (
	inventoryMarker  = "**Rollback（只退 inventory `0009`）**"
	orderingMarker   = "**Rollback（只退 ordering `0008`）**"
	ordering9Marker  = "**Rollback（只退 ordering `0009`）**"
	ordering10Marker = "**Rollback（只退 ordering `0010`）**"
	payment3Marker   = "**Rollback（只退 payment `0003`）**"
	payment4Marker   = "**Rollback（只退 payment `0004`）**"
)

const (
	inventory0009Check = `SELECT count(*) FROM pg_constraint WHERE conrelid = 'inventory.stocks'::regclass AND conname = 'stocks_sku_length'`
	ordering0008Cols   = `SELECT count(*) FROM information_schema.columns WHERE table_schema = 'ordering' AND table_name = 'orders' AND column_name IN ('idempotency_key', 'request_hash')`
	ordering0008Index  = `SELECT count(*) FROM pg_indexes WHERE schemaname = 'ordering' AND indexname = 'orders_customer_idempotency_key'`
	ordering0009Col    = `SELECT count(*) FROM information_schema.columns WHERE table_schema = 'ordering' AND table_name = 'orders' AND column_name = 'payment_required'`
	ordering0010Table  = `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'ordering' AND table_name = 'order_history'`
	ordering0009Tables = `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'ordering' AND table_name IN ('events_v1_orderpaid', 'events_v1_orderpaid_offsets')`
)

var inventory0009Clean = rollbackCase{
	name:    "inventory 0009 from 9 clean",
	runbook: "../../docs/operations.md",
	marker:  inventoryMarker,
	bc:      1,
	latest:  9,
	prepare: func(t *testing.T, e *rollbackEnv) {
		if e.count(t, inventory0009Check) != 1 {
			t.Fatal("0009 did not add stocks_sku_length")
		}
	},
	afterRollback: func(t *testing.T, e *rollbackEnv) {
		if e.count(t, inventory0009Check) != 0 {
			t.Error("the rollback left stocks_sku_length behind")
		}
		if n := e.count(t, `SELECT count(*) FROM inventory.stocks WHERE sku = 'ROLLBACK-STOCK'`); n != 1 {
			t.Errorf("inventory.stocks rows = %d, want 1", n)
		}
	},
	reapply: func(t *testing.T, e *rollbackEnv) {
		if err := postgres.Up(e.c, e.cfg, migrations[1]); err != nil {
			t.Fatal(err)
		}
		if e.count(t, inventory0009Check) != 1 {
			t.Error("up did not re-add the CHECK")
		}
	},
}

var inventory0009Dirty = rollbackCase{
	name:    "inventory 0009 from 9 dirty (no CHECK)",
	runbook: "../../docs/operations.md",
	marker:  inventoryMarker,
	bc:      1,
	latest:  9,
	prepare: func(t *testing.T, e *rollbackEnv) { // a real failed 0009: an over-long row makes ADD CONSTRAINT fail
		if err := postgres.Down(e.c, e.cfg, migrations[1]); err != nil {
			t.Fatal(err)
		}
		e.exec(t, `INSERT INTO inventory.stocks (sku, on_hand, reserved, version, updated_at) VALUES ($1, 1, 0, 1, now())`, strings.Repeat("a", 65))
		if err := postgres.Up(e.c, e.cfg, migrations[1]); err == nil {
			t.Fatal("0009 succeeded over a 65-byte SKU")
		}
		if got := e.version(t, 1); got != "9 dirty=true" {
			t.Fatalf("inventory = %s, want 9 dirty=true", got)
		}
		if e.count(t, inventory0009Check) != 0 {
			t.Fatal("the failed 0009 left a CHECK behind")
		}
	},
	afterRollback: func(t *testing.T, e *rollbackEnv) {
		if e.count(t, inventory0009Check) != 0 {
			t.Error("a CHECK exists after the rollback")
		}
	},
	reapply: func(t *testing.T, e *rollbackEnv) { // the over-long row has to go first, as the runbook says
		e.exec(t, `DELETE FROM inventory.stocks WHERE octet_length(sku) NOT BETWEEN 1 AND 64`)
		if err := postgres.Up(e.c, e.cfg, migrations[1]); err != nil {
			t.Fatal(err)
		}
		if got := e.version(t, 1); got != "9 dirty=false" || e.count(t, inventory0009Check) != 1 {
			t.Errorf("after fixing the data and up: %s, CHECK=%d", got, e.count(t, inventory0009Check))
		}
	},
}

// orderingRunbook is where the ordering 0008 entry is read from.
var orderingRunbook = "../../docs/operations.md"

var ordering0008Clean = rollbackCase{
	name:       "ordering 0008 from 8 clean",
	runbook:    orderingRunbook,
	marker:     orderingMarker,
	legacyDown: "回滾：`migrate down` 一步",
	bc:         0,
	latest:     8,
	prepare: func(t *testing.T, e *rollbackEnv) {
		// ordering is at 10 (order_history) when every BC is at its latest; this runbook is for 8.
		for range 2 { // 10 → 9 → 8
			if err := postgres.Down(e.c, e.cfg, migrations[0]); err != nil {
				t.Fatal(err)
			}
		}
		if e.count(t, ordering0008Cols) != 2 || e.count(t, ordering0008Index) != 1 {
			t.Fatal("0008 did not add its columns and index")
		}
	},
	afterRollback: func(t *testing.T, e *rollbackEnv) {
		if n := e.count(t, ordering0008Cols); n != 0 {
			t.Errorf("idempotency columns left after the rollback: %d", n)
		}
		if n := e.count(t, ordering0008Index); n != 0 {
			t.Errorf("idempotency index left after the rollback: %d", n)
		}
		if n := e.count(t, `SELECT count(*) FROM ordering.orders WHERE id = 'rollback-order'`); n != 1 {
			t.Errorf("ordering.orders rows = %d, want the order kept", n)
		}
	},
	reapply: func(t *testing.T, e *rollbackEnv) {
		if err := postgres.Up(e.c, e.cfg, migrations[0]); err != nil { // re-applies 0008, 0009, then 0010
			t.Fatal(err)
		}
		if e.count(t, ordering0008Cols) != 2 || e.count(t, ordering0008Index) != 1 {
			t.Error("up did not re-add the columns and index")
		}
	},
}

var ordering0008Dirty = rollbackCase{
	name:    "ordering 0008 from 8 dirty (no columns)",
	runbook: orderingRunbook,
	marker:  orderingMarker,
	bc:      0,
	latest:  8,
	prepare: func(t *testing.T, e *rollbackEnv) { // a real failed 0008: ALTER TABLE cannot get its lock within lock_timeout
		for range 3 { // 10 → 9 → 8 → 7: 0008 is the one that fails
			if err := postgres.Down(e.c, e.cfg, migrations[0]); err != nil {
				t.Fatal(err)
			}
		}
		holder := pgtest.Conn(t, e.url) // closed by its t.Cleanup, which runs before the restore above
		tx, err := holder.Begin(e.c)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(e.c, `LOCK TABLE ordering.orders IN ACCESS SHARE MODE`); err != nil {
			t.Fatal(err)
		}
		sep := "?"
		if strings.Contains(e.url, "?") {
			sep = "&"
		}
		short := config.Postgres{URL: e.url + sep + "lock_timeout=200"}
		if err := postgres.Up(e.c, short, migrations[0]); err == nil {
			t.Fatal("0008 got its lock while another session held the table")
		}
		if err := tx.Rollback(e.c); err != nil {
			t.Fatal(err)
		}
		if got := e.version(t, 0); got != "8 dirty=true" {
			t.Fatalf("ordering = %s, want 8 dirty=true", got)
		}
		if e.count(t, ordering0008Cols) != 0 || e.count(t, ordering0008Index) != 0 {
			t.Fatal("the failed 0008 left its columns or index behind")
		}
	},
	afterRollback: func(t *testing.T, e *rollbackEnv) {
		if n := e.count(t, ordering0008Cols); n != 0 {
			t.Errorf("idempotency columns after the rollback: %d", n)
		}
		if n := e.count(t, `SELECT count(*) FROM ordering.orders WHERE id = 'rollback-order'`); n != 1 {
			t.Errorf("ordering.orders rows = %d, want the order kept", n)
		}
	},
	reapply: func(t *testing.T, e *rollbackEnv) { // the lock is free again, so up applies
		if err := postgres.Up(e.c, e.cfg, migrations[0]); err != nil {
			t.Fatal(err)
		}
		if got := e.version(t, 0); got != "10 dirty=false" || e.count(t, ordering0008Cols) != 2 || e.count(t, ordering0008Index) != 1 {
			t.Errorf("after up: %s, columns=%d, index=%d", got, e.count(t, ordering0008Cols), e.count(t, ordering0008Index))
		}
	},
}

var ordering0009Clean = rollbackCase{
	name:    "ordering 0009 from 9 clean",
	runbook: orderingRunbook,
	marker:  ordering9Marker,
	bc:      0,
	latest:  9,
	prepare: func(t *testing.T, e *rollbackEnv) {
		// ordering is at 10 (order_history) when every BC is at its latest; this runbook is for 9.
		if err := postgres.Down(e.c, e.cfg, migrations[0]); err != nil {
			t.Fatal(err)
		}
		if e.count(t, ordering0009Col) != 1 || e.count(t, ordering0009Tables) != 2 {
			t.Fatal("0009 did not add its column and event tables")
		}
		// An order that owes payment: the rollback forgets the snapshot, and says so in the runbook.
		e.exec(t, `UPDATE ordering.orders SET status = 'CONFIRMED', payment_required = true WHERE id = 'rollback-order'`)
	},
	afterRollback: func(t *testing.T, e *rollbackEnv) {
		if n := e.count(t, ordering0009Col); n != 0 {
			t.Errorf("payment_required left after the rollback: %d", n)
		}
		if n := e.count(t, ordering0009Tables); n != 0 {
			t.Errorf("OrderPaid event tables left after the rollback: %d", n)
		}
		if n := e.count(t, `SELECT count(*) FROM ordering.orders WHERE id = 'rollback-order' AND status = 'CONFIRMED'`); n != 1 {
			t.Errorf("the CONFIRMED order did not survive the rollback: %d", n)
		}
	},
	reapply: func(t *testing.T, e *rollbackEnv) {
		if err := postgres.Up(e.c, e.cfg, migrations[0]); err != nil {
			t.Fatal(err)
		}
		if e.count(t, ordering0009Col) != 1 || e.count(t, ordering0009Tables) != 2 {
			t.Error("up did not re-add the column and event tables")
		}
		if n := e.count(t, `SELECT count(*) FROM ordering.orders WHERE id = 'rollback-order' AND NOT payment_required`); n != 1 {
			t.Error("after up the surviving order must be payment_required = false (the default)")
		}
	},
}

var ordering0009Dirty = rollbackCase{
	name:    "ordering 0009 from 9 dirty (no column)",
	runbook: orderingRunbook,
	marker:  ordering9Marker,
	bc:      0,
	latest:  9,
	prepare: func(t *testing.T, e *rollbackEnv) { // a real failed 0009: ALTER TABLE cannot get its lock within lock_timeout
		for range 2 { // 10 → 9 → 8: 0009 is the one that fails
			if err := postgres.Down(e.c, e.cfg, migrations[0]); err != nil {
				t.Fatal(err)
			}
		}
		holder := pgtest.Conn(t, e.url) // closed by its t.Cleanup, which runs before the restore above
		tx, err := holder.Begin(e.c)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(e.c, `LOCK TABLE ordering.orders IN ACCESS SHARE MODE`); err != nil {
			t.Fatal(err)
		}
		sep := "?"
		if strings.Contains(e.url, "?") {
			sep = "&"
		}
		short := config.Postgres{URL: e.url + sep + "lock_timeout=200"}
		if err := postgres.Up(e.c, short, migrations[0]); err == nil {
			t.Fatal("0009 got its lock while another session held the table")
		}
		if err := tx.Rollback(e.c); err != nil {
			t.Fatal(err)
		}
		if got := e.version(t, 0); got != "9 dirty=true" {
			t.Fatalf("ordering = %s, want 9 dirty=true", got)
		}
		if e.count(t, ordering0009Col) != 0 || e.count(t, ordering0009Tables) != 0 {
			t.Fatal("the failed 0009 left its column or event tables behind")
		}
	},
	afterRollback: func(t *testing.T, e *rollbackEnv) {
		if n := e.count(t, ordering0009Col) + e.count(t, ordering0009Tables); n != 0 {
			t.Errorf("0009 objects after the rollback: %d", n)
		}
		if n := e.count(t, `SELECT count(*) FROM ordering.orders WHERE id = 'rollback-order'`); n != 1 {
			t.Errorf("ordering.orders rows = %d, want the order kept", n)
		}
	},
	reapply: func(t *testing.T, e *rollbackEnv) { // the lock is free again, so up applies
		if err := postgres.Up(e.c, e.cfg, migrations[0]); err != nil {
			t.Fatal(err)
		}
		if got := e.version(t, 0); got != "10 dirty=false" || e.count(t, ordering0009Col) != 1 || e.count(t, ordering0009Tables) != 2 {
			t.Errorf("after up: %s, column=%d, tables=%d", got, e.count(t, ordering0009Col), e.count(t, ordering0009Tables))
		}
	},
}

var ordering0010Clean = rollbackCase{
	name:    "ordering 0010 from 10 clean",
	runbook: orderingRunbook,
	marker:  ordering10Marker,
	bc:      0,
	latest:  10,
	prepare: func(t *testing.T, e *rollbackEnv) {
		if e.count(t, ordering0010Table) != 1 || e.count(t, `SELECT count(*) FROM ordering.order_history`) != 1 {
			t.Fatal("0010 did not add order_history, or its sentinel row is missing")
		}
	},
	afterRollback: func(t *testing.T, e *rollbackEnv) {
		if n := e.count(t, ordering0010Table); n != 0 {
			t.Errorf("order_history left after the rollback: %d", n)
		}
		// Only the history goes: the order, its items and the outbox stay.
		if n := e.count(t, `SELECT count(*) FROM ordering.orders WHERE id = 'rollback-order'`); n != 1 {
			t.Errorf("ordering.orders rows = %d, want the order kept", n)
		}
		if n := e.count(t, `SELECT count(*) FROM ordering.order_items WHERE order_id = 'rollback-order'`); n != 1 {
			t.Errorf("ordering.order_items rows = %d, want the items kept", n)
		}
	},
	reapply: func(t *testing.T, e *rollbackEnv) {
		if err := postgres.Up(e.c, e.cfg, migrations[0]); err != nil {
			t.Fatal(err)
		}
		if e.count(t, ordering0010Table) != 1 || e.count(t, `SELECT count(*) FROM ordering.order_history`) != 0 {
			t.Error("up did not re-create an empty order_history")
		}
	},
}

// A dirty 0010 has no table (the file is one transaction): the same SQL only resets the version.
var ordering0010Dirty = rollbackCase{
	name:    "ordering 0010 from 10 dirty (no table)",
	runbook: orderingRunbook,
	marker:  ordering10Marker,
	bc:      0,
	latest:  10,
	prepare: func(t *testing.T, e *rollbackEnv) {
		if err := postgres.Down(e.c, e.cfg, migrations[0]); err != nil { // 10 → 9
			t.Fatal(err)
		}
		// A real failed 0010: CREATE TABLE collides with an object in the way, inside the migration's transaction.
		e.exec(t, `CREATE VIEW ordering.order_history AS SELECT 1 AS x`)
		if err := postgres.Up(e.c, e.cfg, migrations[0]); err == nil {
			t.Fatal("0010 applied over an existing relation of the same name")
		}
		e.exec(t, `DROP VIEW ordering.order_history`)
		if got := e.version(t, 0); got != "10 dirty=true" {
			t.Fatalf("ordering = %s, want 10 dirty=true", got)
		}
		if e.count(t, ordering0010Table) != 0 {
			t.Fatal("the failed 0010 left the table behind")
		}
	},
	afterRollback: func(t *testing.T, e *rollbackEnv) {
		if n := e.count(t, ordering0010Table); n != 0 {
			t.Errorf("order_history after the rollback: %d", n)
		}
		if n := e.count(t, `SELECT count(*) FROM ordering.orders WHERE id = 'rollback-order'`); n != 1 {
			t.Errorf("ordering.orders rows = %d, want the order kept", n)
		}
	},
	reapply: func(t *testing.T, e *rollbackEnv) {
		if err := postgres.Up(e.c, e.cfg, migrations[0]); err != nil {
			t.Fatal(err)
		}
		if got := e.version(t, 0); got != "10 dirty=false" || e.count(t, ordering0010Table) != 1 {
			t.Errorf("after up: %s, table=%d", got, e.count(t, ordering0010Table))
		}
	},
}

const payment0003Tables = `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'payment' AND table_name IN ('mock_checkouts', 'mock_refunds')`

// payment 0003 only adds the mock provider's two tables, so its rollback drops them and nothing else:
// payment.payments and the saga tables keep their rows. (A dirty 0003 is the same SQL: DROP ... IF EXISTS.)
var payment0003Clean = rollbackCase{
	name:    "payment 0003 from 3 clean",
	runbook: "../../docs/operations.md",
	marker:  payment3Marker,
	bc:      4,
	latest:  3,
	prepare: func(t *testing.T, e *rollbackEnv) {
		// payment is at 4 (mock_faults) when every BC is at its latest; this runbook is for 3.
		if err := postgres.Down(e.c, e.cfg, migrations[4]); err != nil {
			t.Fatal(err)
		}
		if e.count(t, payment0003Tables) != 2 || e.count(t, `SELECT count(*) FROM payment.mock_refunds`) != 1 {
			t.Fatal("0003 did not add the mock tables, or the sentinel is missing")
		}
	},
	afterRollback: func(t *testing.T, e *rollbackEnv) {
		if n := e.count(t, payment0003Tables); n != 0 {
			t.Errorf("mock tables left after the rollback: %d", n)
		}
		if n := e.count(t, `SELECT count(*) FROM payment.payments WHERE id = 'payment-sentinel'`) + e.count(t, `SELECT count(*) FROM payment.webhook_events`); n != 2 {
			t.Errorf("payment.payments / webhook_events sentinel rows = %d, want both kept", n)
		}
	},
	reapply: func(t *testing.T, e *rollbackEnv) {
		if err := postgres.Up(e.c, e.cfg, migrations[4]); err != nil {
			t.Fatal(err)
		}
		if got := e.version(t, 4); got != "4 dirty=false" || e.count(t, payment0003Tables) != 2 {
			t.Errorf("after up: %s, mock tables=%d", got, e.count(t, payment0003Tables))
		}
	},
}

// A dirty payment 0003 is a real failed run, in the two shapes the docs promise the same SQL covers. 0003 is one
// transaction of two CREATE TABLEs, so a failure leaves none of its own tables; what a person finds is whatever
// stood in its way. (a) a type named mock_checkouts (a table's row type takes the name): the first CREATE TABLE
// fails, no mock table exists; (b) a table already named mock_refunds: the first CREATE TABLE is rolled back, the
// squatter remains, and the runbook's DROP ... IF EXISTS takes it away.
func failPayment0003(t *testing.T, e *rollbackEnv, standInTheWay string) {
	t.Helper()
	for range 2 { // 0004 (mock_faults), then 0003 itself: the Up below must be the one that runs 0003
		if err := postgres.Down(e.c, e.cfg, migrations[4]); err != nil {
			t.Fatal(err)
		}
	}
	e.exec(t, standInTheWay)
	if err := postgres.Up(e.c, e.cfg, migrations[4]); err == nil {
		t.Fatal("0003 succeeded with something named like its objects already in the way")
	}
	if got := e.version(t, 4); got != "3 dirty=true" {
		t.Fatalf("payment = %s, want 3 dirty=true", got)
	}
}

var payment0003DirtyNoTables = rollbackCase{
	name:    "payment 0003 from 3 dirty (mock tables absent)",
	runbook: "../../docs/operations.md",
	marker:  payment3Marker,
	bc:      4,
	latest:  3,
	prepare: func(t *testing.T, e *rollbackEnv) {
		failPayment0003(t, e, `CREATE TYPE payment.mock_checkouts AS ENUM ('squatter')`)
		if n := e.count(t, payment0003Tables); n != 0 {
			t.Fatalf("the failed 0003 left %d of its tables behind (it is one transaction)", n)
		}
		e.exec(t, `INSERT INTO payment.webhook_events (provider, provider_event_id, event_type, provider_ref, body, received_at) VALUES ('mock', 'sentinel-2', 'payment.captured', 'ref-sentinel', '\x7b7d', now())`) // a non-empty table the snapshot can compare
	},
	afterRollback: func(t *testing.T, e *rollbackEnv) {
		if n := e.count(t, payment0003Tables); n != 0 {
			t.Errorf("mock tables after the rollback: %d", n)
		}
		if n := e.count(t, `SELECT count(*) FROM payment.payments WHERE id = 'payment-sentinel'`); n != 1 {
			t.Errorf("payment.payments rows = %d, want the sentinel kept", n)
		}
	},
	reapply: func(t *testing.T, e *rollbackEnv) { // the squatter is not the runbook's to remove: a person does, then up applies
		e.exec(t, `DROP TYPE payment.mock_checkouts`)
		if err := postgres.Up(e.c, e.cfg, migrations[4]); err != nil {
			t.Fatal(err)
		}
		if got := e.version(t, 4); got != "4 dirty=false" || e.count(t, payment0003Tables) != 2 {
			t.Errorf("after up: %s, mock tables=%d", got, e.count(t, payment0003Tables))
		}
	},
}

var payment0003DirtyTableInTheWay = rollbackCase{
	name:    "payment 0003 from 3 dirty (a table named mock_refunds exists)",
	runbook: "../../docs/operations.md",
	marker:  payment3Marker,
	bc:      4,
	latest:  3,
	prepare: func(t *testing.T, e *rollbackEnv) {
		failPayment0003(t, e, `CREATE TABLE payment.mock_refunds (ref text, idempotency_key text); INSERT INTO payment.mock_refunds VALUES ('squatter', 'k')`)
		if n := e.count(t, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'payment' AND table_name = 'mock_checkouts'`); n != 0 {
			t.Fatal("the failed 0003 left mock_checkouts behind (it is one transaction)")
		}
	},
	afterRollback: func(t *testing.T, e *rollbackEnv) {
		if n := e.count(t, payment0003Tables); n != 0 {
			t.Errorf("mock tables after the rollback (the squatter is DROP ... IF EXISTS'd too): %d", n)
		}
		if n := e.count(t, `SELECT count(*) FROM payment.payments WHERE id = 'payment-sentinel'`); n != 1 {
			t.Errorf("payment.payments rows = %d, want the sentinel kept", n)
		}
	},
	reapply: func(t *testing.T, e *rollbackEnv) {
		if err := postgres.Up(e.c, e.cfg, migrations[4]); err != nil {
			t.Fatal(err)
		}
		if got := e.version(t, 4); got != "4 dirty=false" || e.count(t, payment0003Tables) != 2 {
			t.Errorf("after up: %s, mock tables=%d", got, e.count(t, payment0003Tables))
		}
	},
}

const payment0004Table = `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'payment' AND table_name = 'mock_faults'`

// payment 0004 only adds the mock provider's fault table, so its rollback drops it and nothing else: the mock's
// checkouts and refunds (0003) and every other payment table keep their rows.
var payment0004Clean = rollbackCase{
	name:    "payment 0004 from 4 clean",
	runbook: "../../docs/operations.md",
	marker:  payment4Marker,
	bc:      4,
	latest:  4,
	prepare: func(t *testing.T, e *rollbackEnv) {
		if e.count(t, payment0004Table) != 1 || e.count(t, `SELECT count(*) FROM payment.mock_faults`) != 1 {
			t.Fatal("0004 did not add the fault table, or the sentinel is missing")
		}
	},
	afterRollback: func(t *testing.T, e *rollbackEnv) {
		if n := e.count(t, payment0004Table); n != 0 {
			t.Errorf("mock_faults left after the rollback: %d", n)
		}
		if n := e.count(t, payment0003Tables); n != 2 {
			t.Errorf("mock tables = %d after the 0004 rollback, want 0003 untouched (2)", n)
		}
		if n := e.count(t, `SELECT count(*) FROM payment.mock_refunds`) + e.count(t, `SELECT count(*) FROM payment.payments WHERE id = 'payment-sentinel'`); n != 2 {
			t.Errorf("mock_refunds / payments sentinel rows = %d, want both kept", n)
		}
	},
	reapply: func(t *testing.T, e *rollbackEnv) {
		if err := postgres.Up(e.c, e.cfg, migrations[4]); err != nil {
			t.Fatal(err)
		}
		if got := e.version(t, 4); got != "4 dirty=false" || e.count(t, payment0004Table) != 1 {
			t.Errorf("after up: %s, mock_faults=%d", got, e.count(t, payment0004Table))
		}
	},
}

func TestRollbackRunbooks(t *testing.T) {
	for _, tc := range []rollbackCase{inventory0009Clean, inventory0009Dirty, ordering0010Clean, ordering0010Dirty, ordering0009Clean, ordering0009Dirty, ordering0008Clean, ordering0008Dirty, payment0004Clean, payment0003Clean, payment0003DirtyNoTables, payment0003DirtyTableInTheWay} {
		t.Run(tc.name, func(t *testing.T) { runRollbackCase(t, tc) })
	}
}
