package main

import (
	"bytes"
	"context"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"

	inventorypg "github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	orderingpg "github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// Integration (pgtest): `worker prune` on the real BC tables — the dry run
// reports without deleting, the real run deletes what the platform says.
func TestPruneCommand(t *testing.T) {
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
	// A published-only topic (no cursor): rows there are skipped by default.
	const topic = "inventory.v1.StockReserved"
	for _, tbl := range []string{eventbus.TableName(topic), eventbus.OffsetsTableName(topic), "inventory.inbox"} {
		if err := db.WithContext(c).Exec("TRUNCATE " + tbl).Error; err != nil {
			t.Fatal(err)
		}
	}
	pub, err := eventbus.NewPostgresPublisher(db, tel.Logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.Publish(topic, message.NewMessage(watermill.NewUUID(), []byte("p"))); err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(c).Exec("UPDATE " + eventbus.TableName(topic) + " SET created_at = created_at - interval '48 hours'").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(c).Exec("INSERT INTO inventory.inbox (message_id, consumed_at) VALUES (?, now() - interval '48 hours'), (?, now())", watermill.NewUUID(), watermill.NewUUID()).Error; err != nil {
		t.Fatal(err)
	}
	count := func(tbl string) int64 {
		var n int64
		if err := db.WithContext(c).Raw("SELECT count(*) FROM " + tbl).Scan(&n).Error; err != nil {
			t.Fatal(err)
		}
		return n
	}
	exec := func(cfg *config.AppConfig, args ...string) (string, error) {
		var out bytes.Buffer
		cmd := newRootCmd(func() (*config.AppConfig, error) { return cfg, nil })
		cmd.SetOut(&out)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(append([]string{"prune"}, args...))
		err := cmd.ExecuteContext(c)
		return out.String(), err
	}

	// Argument errors never open the database.
	noDB := &config.AppConfig{Log: cfg.Log}
	for _, args := range [][]string{{}, {"--older-than", "0"}, {"--older-than", "24h", "billing"}, {"--older-than", "30d"}} {
		if _, err := exec(noDB, args...); err == nil || strings.Contains(err.Error(), "postgres.url") {
			t.Fatalf("prune %v without a database = %v, want a usage error", args, err)
		}
	}
	if _, err := exec(noDB, "--older-than", "24h"); err == nil || !strings.Contains(err.Error(), "postgres.url") {
		t.Fatalf("prune without a database = %v", err)
	}

	out, err := exec(cfg, "--older-than", "24h", "--dry-run", "inventory")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	row := func(out, table string, n string) bool {
		return regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(table) + `\s+` + n + `\b`).MatchString(out)
	}
	if !strings.Contains(out, "WOULD DELETE") || !row(out, eventbus.TableName(topic), "0") || !strings.Contains(out, "no consumer group") ||
		!row(out, "inventory.inbox", "1") || !strings.Contains(out, "would delete 1 row(s)") {
		t.Fatalf("dry run output:\n%s", out)
	}
	if count(eventbus.TableName(topic)) != 1 || count("inventory.inbox") != 2 {
		t.Fatal("dry run deleted rows")
	}
	out, err = exec(cfg, "--older-than", "24h", "--unconsumed", "inventory")
	if err != nil {
		t.Fatalf("prune: %v\n%s", err, out)
	}
	if !strings.Contains(out, "DELETED") || !row(out, eventbus.TableName(topic), "1") || !row(out, "inventory.inbox", "1") || !strings.Contains(out, "deleted 2 row(s)") {
		t.Fatalf("prune output:\n%s", out)
	}
	if count(eventbus.TableName(topic)) != 0 || count("inventory.inbox") != 1 {
		t.Fatalf("after prune: topic=%d inbox=%d", count(eventbus.TableName(topic)), count("inventory.inbox"))
	}
}
