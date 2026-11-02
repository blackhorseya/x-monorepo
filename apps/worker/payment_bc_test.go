package main

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/message/router/middleware"
	goredis "github.com/redis/go-redis/v9"

	inventorypg "github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	orderingpg "github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	paymentv1 "github.com/blackhorseya/x-monorepo/pkgs/payment/contract/v1"
	paymentpg "github.com/blackhorseya/x-monorepo/pkgs/payment/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
	platformredis "github.com/blackhorseya/x-monorepo/pkgs/platform/redis"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/redis/redistest"
)

// docs/payment.md §9 (slice 3, poison / prune): payment is a BC the operator tools serve, named by its
// contract's string constant — the worker imports no payment domain code to do it.
func TestSchemas_ServePayment(t *testing.T) {
	if !slices.Contains(schemas, paymentv1.Schema) {
		t.Fatalf("schemas = %v: `worker poison` and `worker prune` do not serve %q", schemas, paymentv1.Schema)
	}
}

// The real tables: a message parked in payment's poison table is listed, shown and dropped by
// `worker poison`, and `worker prune payment` reaches payment's outbox tables and its inbox like any BC's.
func TestPoisonAndPrune_ServePaymentTables(t *testing.T) {
	c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cfg := &config.AppConfig{Postgres: config.Postgres{URL: pgtest.URL(t)}, Log: config.Log{Level: "warn", Format: "text"}}
	tel, telCleanup, err := otel.New(cfg.OTel, cfg.Log)
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	db := pgtest.DB(t, cfg.Postgres, tel)
	for _, m := range []postgres.Migration{orderingpg.Migration(), inventorypg.Migration(), paymentpg.Migration()} {
		if err := postgres.Up(c, cfg.Postgres, m); err != nil {
			t.Fatal(err)
		}
	}
	const refunded = "payment.v1.PaymentRefunded" // published-only: no consumer group has a cursor on it
	for _, tbl := range []string{"payment.events_poison", "payment.inbox", eventbus.TableName(refunded)} {
		if err := db.WithContext(c).Exec("TRUNCATE " + tbl).Error; err != nil {
			t.Fatal(err)
		}
	}
	pub, err := eventbus.NewPostgresPublisher(db, tel.Logger)
	if err != nil {
		t.Fatal(err)
	}
	msg := message.NewMessage(watermill.NewUUID(), []byte("refund me"))
	msg.Metadata.Set(middleware.PoisonedTopicKey, "payment.v1.PaymentRefundRequested")
	msg.Metadata.Set(middleware.PoisonedHandlerKey, "payment.v1.PaymentRefundRequested")
	msg.Metadata.Set(middleware.ReasonForPoisonedKey, "provider down")
	if err := pub.Publish(eventbus.PoisonTopic(paymentv1.Schema), msg); err != nil {
		t.Fatal(err)
	}
	if err := pub.Publish(refunded, message.NewMessage(watermill.NewUUID(), []byte("p"))); err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(c).Exec("UPDATE " + eventbus.TableName(refunded) + " SET created_at = created_at - interval '48 hours'").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(c).Exec("INSERT INTO payment.inbox (message_id, consumed_at) VALUES (?, now() - interval '48 hours')", watermill.NewUUID()).Error; err != nil {
		t.Fatal(err)
	}
	run := func(cmdName string, args ...string) (string, error) {
		var out bytes.Buffer
		cmd := newRootCmd(func() (*config.AppConfig, error) { return cfg, nil })
		cmd.SetOut(&out)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(append([]string{cmdName}, args...))
		err := cmd.ExecuteContext(c)

		return out.String(), err
	}

	if out, err := run("poison", "list", "payment"); err != nil || !strings.Contains(out, msg.UUID) || !strings.Contains(out, "provider down") {
		t.Fatalf("poison list payment = %q, %v", out, err)
	}
	if out, err := run("poison", "list"); err != nil || !strings.Contains(out, msg.UUID) {
		t.Fatalf("poison list (every BC) = %q, %v", out, err)
	}
	out, err := run("prune", "--older-than", "24h", "--dry-run", "payment")
	if err != nil {
		t.Fatalf("prune dry run: %v\n%s", err, out)
	}
	if !regexp.MustCompile(`(?m)^payment\.inbox\s+1\b`).MatchString(out) || !strings.Contains(out, "payment.events_v1_paymentrefunded") {
		t.Fatalf("prune payment did not reach payment's inbox and outbox tables:\n%s", out)
	}
	if out, err := run("prune", "--older-than", "24h", "--unconsumed", "payment"); err != nil || !strings.Contains(out, "DELETED") {
		t.Fatalf("prune payment = %q, %v", out, err)
	}
	var left int64
	if err := db.WithContext(c).Raw("SELECT (SELECT count(*) FROM payment.inbox) + (SELECT count(*) FROM " + eventbus.TableName(refunded) + ")").Scan(&left).Error; err != nil || left != 0 {
		t.Fatalf("rows left after prune = %d, %v", left, err)
	}
	if out, err := run("poison", "drop", "payment", "all"); err != nil || !strings.Contains(out, "dropped 1 message(s)") {
		t.Fatalf("poison drop payment = %q, %v", out, err)
	}
}

// docs/payment.md §1: apps/worker consumes payment's events through their wire format and loads none of
// payment's code — no domain, application, infrastructure, adapters, and not the BC's provider (which is
// where its provider credentials are read). Transitive, over the real import graph of the binary, with a
// positive control: payment's contract IS linked (the events are decoded), or the rule would pass on a
// worker that has no payment at all.
func TestWorker_LinksPaymentContractOnly(t *testing.T) {
	const mod = "github.com/blackhorseya/x-monorepo/pkgs/payment"
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	var contract bool
	for _, p := range strings.Fields(string(out)) {
		switch {
		case p == mod+"/contract/v1":
			contract = true
		case p == mod || strings.HasPrefix(p, mod+"/"):
			t.Errorf("apps/worker links %s: it may link only payment's contract/v1 (and never the BC's provider or its credentials)", p)
		}
	}
	if !contract {
		t.Errorf("apps/worker does not link %s/contract/v1 (positive control)", mod)
	}
}

// docs/operations.md, "payment tables are missing": a worker started on a database whose payment
// migration was not applied refuses to start — before it consumes anything — and names the table and
// the way out, instead of running the other BCs' handlers beside two subscribers that fail on every poll.
func TestNewWorker_RefusesAMissingPaymentTable(t *testing.T) {
	c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pgCfg := config.Postgres{URL: pgtest.URL(t)}
	db := pgtest.DB(t, pgCfg, pgtest.Telemetry(t))
	// Only ordering and inventory are migrated: the state of a database upgraded to this worker
	// before `migrate up` reached payment. Drop whatever an earlier test in the package left.
	if err := db.WithContext(c).Exec(`DROP SCHEMA IF EXISTS payment CASCADE`).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // leave the schema as every other test in the package expects it
		if err := postgres.Up(context.Background(), pgCfg, paymentpg.Migration()); err != nil {
			t.Errorf("restore payment: %v", err)
		}
	})

	for _, broker := range []string{config.BrokerPostgres, config.BrokerRedis} {
		t.Run(broker, func(t *testing.T) {
			var rdb goredis.UniversalClient
			if broker == config.BrokerRedis {
				client, cleanup, err := platformredis.NewClient(config.Redis{URL: redistest.URL(t)})
				if err != nil {
					t.Fatal(err)
				}
				defer cleanup()
				rdb = client
			}
			w, err := newWorkerMigrated(t, broker, rdb, 0, orderingpg.Migration(), inventorypg.Migration())
			if err == nil {
				t.Fatalf("NewWorker started on a database without payment's tables: %v", w)
			}
			for _, want := range []string{"payment.events_v1_paymentcaptured", "payment.events_v1_paymentexpired", "migrate"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
		})
	}
}
