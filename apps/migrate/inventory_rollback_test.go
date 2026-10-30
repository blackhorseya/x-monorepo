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

// docRollbackSQL is the ```sql block that follows the inventory 0009 rollback marker in
// docs/operations.md: the statements an operator is told to run, not a copy of them.
func docRollbackSQL(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../docs/operations.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	const marker = "**Rollback（只退 inventory `0009`）**"
	i := strings.Index(doc, marker)
	if i < 0 {
		t.Fatalf("docs/operations.md has no %q section", marker)
	}
	m := regexp.MustCompile("(?s)```sql\n(.*?)```").FindStringSubmatch(doc[i:])
	if m == nil {
		t.Fatal("no ```sql block after the inventory 0009 rollback marker")
	}

	return m[1]
}

// The documented inventory 0009 rollback (docs/operations.md) moves only inventory: the CHECK
// goes, inventory is version 8 clean, and catalog / identity / ordering keep their version,
// schema and rows. `migrate down` must not be the answer — it steps every BC back.
func TestInventory0009Rollback_LeavesOtherBCsAlone(t *testing.T) {
	url := pgtest.URL(t)
	c := context.Background()
	cfg := config.Postgres{URL: url}

	for _, m := range migrations { // bring every BC to its latest
		if err := postgres.Up(c, cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := pgx.Connect(c, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(c)

	versions := func() map[string]string {
		t.Helper()
		got := map[string]string{}
		for _, m := range migrations {
			v, dirty, err := postgres.Version(c, cfg, m)
			if err != nil {
				t.Fatal(err)
			}
			got[m.Schema] = fmt.Sprintf("%d dirty=%v", v, dirty)
		}

		return got
	}
	count := func(q string) int {
		t.Helper()
		var n int
		if err := conn.QueryRow(c, q).Scan(&n); err != nil {
			t.Fatal(err)
		}

		return n
	}

	if _, err := conn.Exec(c, `TRUNCATE catalog.products, inventory.stocks CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(c, `INSERT INTO catalog.products (sku, name, price_amount, price_currency, active, updated_at) VALUES ('ROLLBACK-SENTINEL', 'sentinel', 100, 'USD', true, now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(c, `INSERT INTO inventory.stocks (sku, on_hand, reserved, version, updated_at) VALUES ('ROLLBACK-STOCK', 3, 0, 1, now())`); err != nil {
		t.Fatal(err)
	}
	before := versions()
	const checkQ = `SELECT count(*) FROM pg_constraint WHERE conrelid = 'inventory.stocks'::regclass AND conname = 'stocks_sku_length'`
	if count(checkQ) != 1 {
		t.Fatal("0009 did not add stocks_sku_length")
	}
	tables := count(`SELECT count(*) FROM information_schema.tables WHERE table_schema IN ('catalog','identity','ordering','inventory')`)

	if _, err := conn.Exec(c, docRollbackSQL(t)); err != nil {
		t.Fatalf("documented rollback: %v", err)
	}

	after := versions()
	for schema, v := range before {
		want := v
		if schema == "inventory" {
			want = "8 dirty=false"
		}
		if after[schema] != want {
			t.Errorf("%s: %s, want %s", schema, after[schema], want)
		}
	}
	if count(checkQ) != 0 {
		t.Error("the rollback left stocks_sku_length behind")
	}
	if n := count(`SELECT count(*) FROM catalog.products WHERE sku = 'ROLLBACK-SENTINEL'`); n != 1 {
		t.Errorf("catalog.products sentinel rows = %d, want 1", n)
	}
	if n := count(`SELECT count(*) FROM inventory.stocks WHERE sku = 'ROLLBACK-STOCK'`); n != 1 {
		t.Errorf("inventory.stocks rows = %d, want 1", n)
	}
	if n := count(`SELECT count(*) FROM information_schema.tables WHERE table_schema IN ('catalog','identity','ordering','inventory')`); n != tables {
		t.Errorf("tables across the BC schemas: %d, want %d", n, tables)
	}

	// and 0009 applies again
	if err := postgres.Up(c, cfg, migrations[1]); err != nil {
		t.Fatal(err)
	}
	if count(checkQ) != 1 {
		t.Error("up did not re-add the CHECK")
	}
}
