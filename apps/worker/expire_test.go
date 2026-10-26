package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	inventorypg "github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	orderingpg "github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// Integration (pgtest): `worker expire` on the real tables — a dry run lists
// without touching, the real run releases the stock, ends the reservation
// EXPIRED and leaves the cancellation request in ordering's outbox table.
func TestExpireCommand(t *testing.T) {
	c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cfg := &config.AppConfig{Postgres: config.Postgres{URL: pgtest.URL(t)}, Log: config.Log{Level: "warn", Format: "text"}}
	tel, telCleanup, err := otel.New(cfg.OTel, cfg.Log)
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	db, dbCleanup, err := postgres.NewDB(cfg.Postgres, tel)
	if err != nil {
		t.Fatal(err)
	}
	defer dbCleanup()
	for _, m := range []postgres.Migration{orderingpg.Migration(), inventorypg.Migration()} {
		if err := postgres.Up(c, cfg.Postgres, m); err != nil {
			t.Fatal(err)
		}
	}
	for _, tbl := range []string{"inventory.reservations", "inventory.stocks", "ordering.events_v1_ordercancellationrequested", "inventory.events_v1_stockreleased"} {
		if err := db.WithContext(c).Exec("TRUNCATE " + tbl + " CASCADE").Error; err != nil {
			t.Fatal(err)
		}
	}
	// One SKU, three holds: stale (2 days), fresh (now), and one already committed
	// a week ago (terminal, never due). Written as the repositories would.
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := db.WithContext(c).Exec("INSERT INTO inventory.stocks (sku, on_hand, reserved, version, updated_at) VALUES ('S', 10, 2, 2, ?)", now).Error; err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		id, status string
		at         time.Time
	}{{"o-stale", "RESERVED", now.Add(-48 * time.Hour)}, {"o-fresh", "RESERVED", now}, {"o-sold", "COMMITTED", now.Add(-7 * 24 * time.Hour)}} {
		if err := db.WithContext(c).Exec("INSERT INTO inventory.reservations (order_id, status, created_at, updated_at) VALUES (?, ?, ?, ?)", r.id, r.status, r.at, r.at).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.WithContext(c).Exec("INSERT INTO inventory.reservation_lines (order_id, position, sku, quantity, short) VALUES (?, 0, 'S', 1, false)", r.id).Error; err != nil {
			t.Fatal(err)
		}
	}
	count := func(tbl, where string) int64 {
		var n int64
		if err := db.WithContext(c).Raw("SELECT count(*) FROM " + tbl + " WHERE " + where).Scan(&n).Error; err != nil {
			t.Fatal(err)
		}
		return n
	}
	exec := func(cfg *config.AppConfig, args ...string) (string, error) {
		var out bytes.Buffer
		cmd := newRootCmd(func() (*config.AppConfig, error) { return cfg, nil })
		cmd.SetOut(&out)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(append([]string{"expire"}, args...))
		err := cmd.ExecuteContext(c)
		return out.String(), err
	}

	// Argument errors never open the database.
	noDB := &config.AppConfig{Log: cfg.Log}
	for _, args := range [][]string{{}, {"--older-than", "0"}, {"--older-than", "24h", "--batch", "0"}, {"--older-than", "24h", "inventory"}} {
		if _, err := exec(noDB, args...); err == nil || strings.Contains(err.Error(), "postgres.url") {
			t.Fatalf("expire %v without a database = %v, want a usage error", args, err)
		}
	}
	if _, err := exec(noDB, "--older-than", "24h"); err == nil || !strings.Contains(err.Error(), "postgres.url") {
		t.Fatalf("expire without a database = %v", err)
	}

	out, err := exec(cfg, "--older-than", "24h", "--dry-run")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "WOULD EXPIRE") || !strings.Contains(out, "o-stale") || strings.Contains(out, "o-fresh") || strings.Contains(out, "o-sold") ||
		!strings.Contains(out, "would expire 1 reservation(s) held longer than 24h0m0s") {
		t.Fatalf("dry run output:\n%s", out)
	}
	if count("inventory.reservations", "status = 'RESERVED'") != 2 || count("inventory.stocks", "sku = 'S' AND reserved = 2") != 1 {
		t.Fatal("dry run changed state")
	}

	// --batch 1 still sweeps everything due: the loop runs until a batch comes back short.
	out, err = exec(cfg, "--older-than", "24h", "--batch", "1")
	if err != nil {
		t.Fatalf("expire: %v\n%s", err, out)
	}
	if !strings.Contains(out, "o-stale") || !strings.Contains(out, "expired 1 reservation(s)") {
		t.Fatalf("expire output:\n%s", out)
	}
	if count("inventory.reservations", "order_id = 'o-stale' AND status = 'EXPIRED'") != 1 ||
		count("inventory.reservations", "order_id = 'o-fresh' AND status = 'RESERVED'") != 1 ||
		count("inventory.reservations", "order_id = 'o-sold' AND status = 'COMMITTED'") != 1 {
		t.Fatal("wrong reservations changed")
	}
	// The stock went back (reserved 2 → 1, on_hand untouched, version advanced) and both
	// events sit in their outbox tables: StockReleased in inventory's, the cancellation
	// request — ordering's message — in ordering's.
	if count("inventory.stocks", "sku = 'S' AND on_hand = 10 AND reserved = 1 AND version = 3") != 1 {
		t.Fatal("stock not released")
	}
	if count("inventory.events_v1_stockreleased", "metadata->>'key' = 'o-stale'") != 1 ||
		count("ordering.events_v1_ordercancellationrequested", "metadata->>'key' = 'o-stale' AND encode(payload, 'escape') LIKE '%reservation expired after 48h%'") != 1 {
		t.Fatal("events not published")
	}
	// Nothing due any more.
	out, err = exec(cfg, "--older-than", "24h")
	if err != nil || !strings.Contains(out, "expired 0 reservation(s)") {
		t.Fatalf("second run: %v\n%s", err, out)
	}
}
