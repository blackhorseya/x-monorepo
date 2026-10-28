package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// FindByOrderIDs reads 100 reservations (and their lines) by primary key: both
// queries must be index scans, never a Seq Scan over a table that grows with
// every order. No migration is needed because reservations(order_id) and
// reservation_lines(order_id, position) are already primary keys.
func TestReservationRepository_FindByOrderIDsUsesIndex(t *testing.T) {
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
	if err := db.WithContext(c).Exec("TRUNCATE inventory.reservations CASCADE").Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("TRUNCATE inventory.reservations CASCADE") })

	// 20k reservations with 2 lines each, so the planner sees a selective batch; ANALYZE makes the stats current.
	if err := db.WithContext(c).Exec(`
		INSERT INTO inventory.reservations (order_id, status, created_at, updated_at)
		SELECT 'o-' || lpad(g::text, 6, '0'), 'RESERVED', now(), now() FROM generate_series(1, 20000) g`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(c).Exec(`
		INSERT INTO inventory.reservation_lines (order_id, position, sku, quantity, short)
		SELECT 'o-' || lpad(g::text, 6, '0'), p, 'SKU-' || p, 1, false FROM generate_series(1, 20000) g, generate_series(0, 1) p`).Error; err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"reservations", "reservation_lines"} {
		if err := db.WithContext(c).Exec("ANALYZE inventory." + tbl).Error; err != nil {
			t.Fatal(err)
		}
	}

	ids := make([]string, 100)
	for i := range ids {
		ids[i] = fmt.Sprintf("o-%06d", 100+i*150)
	}

	// Same shapes as ReservationRepository.FindByOrderIDs: the IN lookup, then the lines preload.
	for tbl, query := range map[string]string{
		"reservations":      "SELECT * FROM inventory.reservations WHERE order_id IN ?",
		"reservation_lines": "SELECT * FROM inventory.reservation_lines WHERE order_id IN ? ORDER BY position",
	} {
		var raw string
		if err := db.WithContext(c).Raw("EXPLAIN (FORMAT JSON) "+query, ids).Scan(&raw).Error; err != nil {
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
			// A bitmap scan names its index on the child node, which carries no relation name.
			if n.IndexName != "" && strings.HasPrefix(n.IndexName, tbl) {
				indexScan = true
			}
			if n.RelationName == tbl && n.NodeType == "Seq Scan" {
				seqScan = true
			}
		})
		if seqScan || !indexScan {
			t.Fatalf("%s plan: seq scan = %v, index scan = %v, want index only\n%s", tbl, seqScan, indexScan, raw)
		}
	}
}
