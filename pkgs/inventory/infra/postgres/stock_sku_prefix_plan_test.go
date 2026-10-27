package postgres_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// planNode is the subset of an EXPLAIN (FORMAT JSON) node this test reads.
type planNode struct {
	NodeType     string     `json:"Node Type"`
	RelationName string     `json:"Relation Name"`
	IndexName    string     `json:"Index Name"`
	Plans        []planNode `json:"Plans"`
}

func (n planNode) walk(fn func(planNode)) {
	fn(n)
	for _, c := range n.Plans {
		c.walk(fn)
	}
}

// ListStocks' SKU prefix filter must be served by an index: the primary key's
// default-collation btree cannot answer LIKE 'prefix%', so without a
// text_pattern_ops index every filtered page is a Seq Scan over inventory.stocks.
func TestStockRepository_SKUPrefixUsesIndex(t *testing.T) {
	url := pgtest.URL(t)

	c := context.Background()
	cfg := config.Postgres{URL: url}
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	db, cleanup, err := platformpg.NewDB(cfg, tel)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	if err := platformpg.Up(c, cfg, postgres.Migration()); err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(c).Exec("TRUNCATE inventory.stocks CASCADE").Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("TRUNCATE inventory.stocks CASCADE") })

	// 20k rows so the planner sees a selective prefix; ANALYZE makes the stats current.
	if err := db.WithContext(c).Exec(`
		INSERT INTO inventory.stocks (sku, on_hand, reserved, version, updated_at)
		SELECT 'SKU-' || lpad(g::text, 6, '0'), 10, 0, 1, now() FROM generate_series(1, 20000) g`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(c).Exec("ANALYZE inventory.stocks").Error; err != nil {
		t.Fatal(err)
	}

	// Same shape as StockRepository.List: LIKE ... ESCAPE, keyset bound, ORDER BY sku, LIMIT page+1.
	var raw string
	err = db.WithContext(c).Raw(
		`EXPLAIN (FORMAT JSON) SELECT * FROM inventory.stocks
		 WHERE sku LIKE ? ESCAPE '\' ORDER BY sku ASC LIMIT 51`, "SKU-0123%").Scan(&raw).Error
	if err != nil {
		t.Fatal(err)
	}
	var plans []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(raw), &plans); err != nil || len(plans) != 1 {
		t.Fatalf("parse plan %q: %v", raw, err)
	}

	var seqScan, indexScan bool
	plans[0].Plan.walk(func(n planNode) {
		if n.RelationName != "stocks" {
			return
		}
		switch {
		case n.NodeType == "Seq Scan":
			seqScan = true
		case strings.Contains(n.NodeType, "Index"):
			indexScan = true
		}
	})
	if seqScan || !indexScan {
		t.Fatalf("sku prefix plan: seq scan = %v, index scan = %v, want index only\n%s", seqScan, indexScan, raw)
	}
}
