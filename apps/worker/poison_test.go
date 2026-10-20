package main

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/message/router/middleware"

	inventorypg "github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	orderingpg "github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// Integration (pgtest): `worker poison` against the real ordering tables.
func TestPoisonCommand(t *testing.T) {
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
	const topic = "ordering.v1.OrderCancellationRequested"
	for _, tbl := range []string{"ordering.events_poison", eventbus.TableName(topic)} {
		if err := db.WithContext(c).Exec("TRUNCATE " + tbl).Error; err != nil {
			t.Fatal(err)
		}
	}
	pub, err := eventbus.NewPostgresPublisher(db, tel.Logger)
	if err != nil {
		t.Fatal(err)
	}
	park := func(payload string) string {
		msg := message.NewMessage(watermill.NewUUID(), []byte(payload))
		msg.Metadata.Set(middleware.PoisonedTopicKey, topic)
		msg.Metadata.Set(middleware.PoisonedHandlerKey, topic)
		msg.Metadata.Set(middleware.ReasonForPoisonedKey, "boom:\n  db down")
		if err := pub.Publish(eventbus.PoisonTopic("ordering"), msg); err != nil {
			t.Fatal(err)
		}
		return msg.UUID
	}
	// Every invocation is a fresh CLI, as a shell would run it.
	exec := func(cfg *config.AppConfig, args ...string) (string, error) {
		var out bytes.Buffer
		cmd := newRootCmd(func() (*config.AppConfig, error) { return cfg, nil })
		cmd.SetOut(&out)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(append([]string{"poison"}, args...))
		err := cmd.ExecuteContext(c)
		return out.String(), err
	}
	poison := func(args ...string) (string, error) { return exec(cfg, args...) }

	// Usage and argument errors never touch the database: they fail the same
	// way with no postgres.url.
	noDB := &config.AppConfig{Log: cfg.Log}
	for _, args := range [][]string{{"bogus"}, {"list", "billing"}, {"requeue", "ordering"}, {"drop", "ordering", "x"}, {"show", "ordering"}, {"show", "ordering", "1", "2"}} {
		if _, err := exec(noDB, args...); err == nil || strings.Contains(err.Error(), "postgres.url") {
			t.Fatalf("poison %v without a database = %v, want a usage error", args, err)
		}
	}
	if _, err := exec(noDB, "list"); err == nil || !strings.Contains(err.Error(), "postgres.url") {
		t.Fatalf("list without a database = %v", err)
	}
	if out, err := poison("--help"); err != nil || !strings.Contains(out, "requeue") {
		t.Fatalf("help = %q, %v", out, err)
	}

	one, two := park("one"), park("two")
	out, err := poison("list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, one) || !strings.Contains(out, two) || !strings.Contains(out, "boom: db down") || strings.Contains(out, "\n  db") {
		t.Fatalf("list = %q", out)
	}
	if out, err := poison("list", "-n", "1", "inventory"); err != nil || strings.Contains(out, one) {
		t.Fatalf("list inventory = %q, %v", out, err)
	}
	rows, err := eventbus.ListPoison(c, db, eventbus.PoisonTopic("ordering"), 0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %v, %v", rows, err)
	}
	first := rows[0].Offset

	out, err = poison("show", "ordering", itoa(first))
	if err != nil || !strings.Contains(out, "uuid:      "+one) || !strings.Contains(out, "payload:   3 bytes") || !strings.Contains(out, "b25l") {
		t.Fatalf("show = %q, %v", out, err)
	}
	if _, err := poison("show", "ordering", "999999"); err == nil {
		t.Fatal("show of a missing row should fail")
	}

	// requeue one → it is back on its topic under the same uuid.
	if out, err := poison("requeue", "ordering", itoa(first)); err != nil || !strings.Contains(out, "requeued 1 message(s)") {
		t.Fatalf("requeue = %q, %v", out, err)
	}
	var n int64
	if err := db.WithContext(c).Raw("SELECT count(*) FROM "+eventbus.TableName(topic)+" WHERE uuid = ?", one).Scan(&n).Error; err != nil || n != 1 {
		t.Fatalf("requeued rows = %d, %v", n, err)
	}
	if out, err := poison("drop", "ordering", "all"); err != nil || !strings.Contains(out, "dropped 1 message(s)") {
		t.Fatalf("drop = %q, %v", out, err)
	}
	if out, err := poison("list", "ordering"); err != nil || strings.Contains(out, two) {
		t.Fatalf("list after drop = %q, %v", out, err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
