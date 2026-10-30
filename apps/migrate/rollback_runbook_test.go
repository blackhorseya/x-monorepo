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

// runbookEntry runs the rollback entry the runbook file gives.
func (e *rollbackEnv) runbookEntry(t *testing.T, tc rollbackCase) {
	t.Helper()
	b, err := os.ReadFile(tc.runbook)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	switch {
	case strings.Contains(doc, tc.marker):
		m := regexp.MustCompile("(?s)```sql\n(.*?)```").FindStringSubmatch(doc[strings.Index(doc, tc.marker):])
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
	conn, err := pgx.Connect(e.c, url)
	if err != nil {
		t.Fatal(err)
	}
	e.conn = conn
	t.Cleanup(func() { _ = conn.Close(e.c) }) // registered first: runs after the restore below
	t.Cleanup(func() {                        // leave every BC applied and clean for the next test
		ctx := context.Background()
		for _, m := range migrations {
			_, _ = conn.Exec(ctx, fmt.Sprintf(`UPDATE %s.schema_migrations SET version = version - 1, dirty = false WHERE dirty`, pgx.Identifier{m.Schema}.Sanitize()))
			_ = postgres.Up(ctx, e.cfg, m)
		}
	})

	// a row in each BC that has one to lose
	e.exec(t, `TRUNCATE catalog.products, inventory.stocks, ordering.orders CASCADE`)
	e.exec(t, `INSERT INTO catalog.products (sku, name, price_amount, price_currency, active, updated_at) VALUES ('ROLLBACK-SENTINEL', 'sentinel', 100, 'USD', true, now())`)
	e.exec(t, `INSERT INTO inventory.stocks (sku, on_hand, reserved, version, updated_at) VALUES ('ROLLBACK-STOCK', 3, 0, 1, now())`)
	e.exec(t, `INSERT INTO ordering.orders (id, customer_id, status, created_at, updated_at) VALUES ('rollback-order', 'rollback-customer', 'PENDING', now(), now())`)

	if tc.prepare != nil {
		tc.prepare(t, e)
	}

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
	inventoryMarker = "**Rollback（只退 inventory `0009`）**"
	orderingMarker  = "**Rollback（只退 ordering `0008`）**"
)

const (
	inventory0009Check = `SELECT count(*) FROM pg_constraint WHERE conrelid = 'inventory.stocks'::regclass AND conname = 'stocks_sku_length'`
	ordering0008Cols   = `SELECT count(*) FROM information_schema.columns WHERE table_schema = 'ordering' AND table_name = 'orders' AND column_name IN ('idempotency_key', 'request_hash')`
	ordering0008Index  = `SELECT count(*) FROM pg_indexes WHERE schemaname = 'ordering' AND indexname = 'orders_customer_idempotency_key'`
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
		if err := postgres.Up(e.c, e.cfg, migrations[0]); err != nil {
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
		if err := postgres.Down(e.c, e.cfg, migrations[0]); err != nil {
			t.Fatal(err)
		}
		holder, err := pgx.Connect(e.c, e.url)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = holder.Close(e.c) }()
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
		if got := e.version(t, 0); got != "8 dirty=false" || e.count(t, ordering0008Cols) != 2 || e.count(t, ordering0008Index) != 1 {
			t.Errorf("after up: %s, columns=%d, index=%d", got, e.count(t, ordering0008Cols), e.count(t, ordering0008Index))
		}
	},
}

func TestRollbackRunbooks(t *testing.T) {
	for _, tc := range []rollbackCase{inventory0009Clean, inventory0009Dirty, ordering0008Clean, ordering0008Dirty} {
		t.Run(tc.name, func(t *testing.T) { runRollbackCase(t, tc) })
	}
}
