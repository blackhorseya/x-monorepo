package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"google.golang.org/protobuf/proto"
	"gorm.io/gorm"

	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	orderingpg "github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	paymentpg "github.com/blackhorseya/x-monorepo/pkgs/payment/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	commonv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/common/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/idgen"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

const (
	pgPass     = "s3cretpgpass"
	mockSecret = "mock-webhook-secret-0123456789"
	topicConf  = "ordering.v1.OrderConfirmed"
)

func s(v string) *string { return &v }

// workerDB is a database with payment's and ordering's schemas migrated (ordering publishes the events payment
// consumes, so the consumer puts its cursor on ordering's event tables), emptied before and after the test.
func workerDB(t *testing.T) (config.Postgres, *gorm.DB) {
	t.Helper()
	pg := config.Postgres{URL: pgtest.URL(t)}
	c := context.Background()
	if err := postgres.Up(c, pg, paymentpg.Migration()); err != nil {
		t.Fatalf("migrate payment: %v", err)
	}
	if err := postgres.Up(c, pg, orderingpg.Migration()); err != nil {
		t.Fatalf("migrate ordering: %v", err)
	}
	db := pgtest.DB(t, pg, pgtest.Telemetry(t))
	topics := []string{topicConf, "ordering.v1.OrderCancelled", "ordering.v1.OrderCompleted", "payment.v1.PaymentCaptured",
		"payment.v1.PaymentExpired", "payment.v1.PaymentRefunded", "payment.v1.PaymentRefundRequested", "payment.v1.WebhookReceived", "payment.poison"}
	clean := func() {
		stmts := []string{"TRUNCATE payment.payments", "TRUNCATE payment.order_outcomes", "TRUNCATE payment.inbox", "TRUNCATE payment.webhook_events"}
		for _, topic := range topics {
			stmts = append(stmts, "TRUNCATE "+eventbus.TableName(topic)+", "+eventbus.OffsetsTableName(topic))
		}
		for _, q := range stmts {
			if err := db.Exec(q).Error; err != nil {
				t.Errorf("%s: %v", q, err)
			}
		}
	}
	clean()
	t.Cleanup(clean)

	return pg, db
}

// durableEnv is a configuration --require-durable accepts; each case overrides one thing. A nil value unsets.
func durableEnv(pgURL string, override map[string]*string) map[string]*string {
	env := map[string]*string{
		"APP_POSTGRES_URL":                s(pgURL),
		"APP_PAYMENT_MOCK_WEBHOOK_SECRET": s(mockSecret),
		"APP_WORKER_POLL_INTERVAL":        s("20ms"),
	}
	for k, v := range override {
		env[k] = v
	}

	return env
}

// outcome of one run(args): it returned on its own (a refusal, or a clean exit), or it was still running after
// a moment and was told to stop like SIGTERM would (served).
type outcome struct {
	err    error
	served bool
}

// startAndStop runs the real entry point. The consumer has no readyz: if run returns on its own that is the
// outcome; if it is still running after the window it is stopped with SIGTERM (repeatedly: a signal sent before
// NotifyContext was installed is lost) and its result is the outcome. A refusal that does not happen therefore
// fails the test instead of hanging it.
func startAndStop(t *testing.T, args []string, env map[string]*string, window time.Duration) outcome {
	t.Helper()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	defer signal.Stop(sig)
	t.Setenv("APP_LOG_LEVEL", "error")
	for k, v := range env {
		if v == nil {
			old, had := os.LookupEnv(k)
			_ = os.Unsetenv(k)
			t.Cleanup(func() {
				if had {
					_ = os.Setenv(k, old)
				}
			})

			continue
		}
		t.Setenv(k, *v)
	}

	done := make(chan error, 1)
	go func() { done <- run(args) }()
	select {
	case err := <-done:
		return outcome{err: err}
	case <-time.After(window):
	}
	for {
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		select {
		case err := <-done:
			return outcome{err: err, served: true}
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// The refusal matrix, read off `payment-rpc consume [--require-durable]` (apps/payment-rpc/main.go, consume.go,
// app.go, pkgs/payment/provider.go) cell by cell. Every cell drives the real run(); a process that starts
// anyway is stopped and the test fails. A refusal never prints a secret and never echoes an argument.
func TestRun_Refuses(t *testing.T) {
	pg, _ := workerDB(t)
	const badArg = "--require-durabel-xyzzy"
	for name, tc := range map[string]struct {
		args []string
		env  map[string]*string
		want string
	}{
		// the command line
		"an unknown flag":                 {[]string{"--nope"}, nil, "unknown argument"},
		"a typo of the flag":              {[]string{badArg}, nil, "unknown argument"},
		"a subcommand (consume)":          {[]string{"consume"}, nil, "unknown argument"},
		"a stray argument after the flag": {[]string{"--require-durable", "extra"}, nil, "unknown argument"},
		// postgres.url: refused with or without the flag (a consumer without a database has nothing to read)
		"no database (unset)":          {nil, map[string]*string{"APP_POSTGRES_URL": nil}, "postgres.url"},
		"no database (empty)":          {nil, map[string]*string{"APP_POSTGRES_URL": s("")}, "postgres.url"},
		"no database (blank)":          {nil, map[string]*string{"APP_POSTGRES_URL": s("   ")}, "postgres.url"},
		"no database, durable (unset)": {[]string{"--require-durable"}, map[string]*string{"APP_POSTGRES_URL": nil}, "postgres.url"},
		"no database, durable (empty)": {[]string{"--require-durable"}, map[string]*string{"APP_POSTGRES_URL": s("")}, "postgres.url"},
		"no database, durable (blank)": {[]string{"--require-durable"}, map[string]*string{"APP_POSTGRES_URL": s("   ")}, "postgres.url"},
		// the broker: only the Postgres outbox exists for payment
		"a redis broker":          {nil, map[string]*string{"APP_WORKER_BROKER": s("redis")}, "worker.broker"},
		"a redis broker, durable": {[]string{"--require-durable"}, map[string]*string{"APP_WORKER_BROKER": s("redis")}, "worker.broker"},
		// --require-durable with the mock provider: the webhook secret is a real value
		"mock webhook secret unset, durable":       {[]string{"--require-durable"}, map[string]*string{"APP_PAYMENT_MOCK_WEBHOOK_SECRET": nil}, "mock_webhook_secret"},
		"mock webhook secret empty, durable":       {[]string{"--require-durable"}, map[string]*string{"APP_PAYMENT_MOCK_WEBHOOK_SECRET": s("")}, "mock_webhook_secret"},
		"mock webhook secret blank, durable":       {[]string{"--require-durable"}, map[string]*string{"APP_PAYMENT_MOCK_WEBHOOK_SECRET": s("   ")}, "mock_webhook_secret"},
		"mock webhook secret tab/newline, durable": {[]string{"--require-durable"}, map[string]*string{"APP_PAYMENT_MOCK_WEBHOOK_SECRET": s("\t\n")}, "mock_webhook_secret"},
		// payment.* validated before anything is opened (pkgs/payment BuildProvider), with or without the flag
		"an unknown provider":                    {nil, map[string]*string{"APP_PAYMENT_PROVIDER": s("stripe")}, "payment.provider"},
		"reconcile window shorter than the page": {nil, map[string]*string{"APP_PAYMENT_RECONCILE_WINDOW": s("10m"), "APP_PAYMENT_MOCK_CHECKOUT_TTL": s("15m"), "APP_PAYMENT_WINDOW": s("10m")}, "payment.reconcile_window"},
		"reconcile window equal to the page":     {nil, map[string]*string{"APP_PAYMENT_RECONCILE_WINDOW": s("15m"), "APP_PAYMENT_MOCK_CHECKOUT_TTL": s("15m"), "APP_PAYMENT_WINDOW": s("10m")}, "payment.reconcile_window"},
		"no reconcile window":                    {nil, map[string]*string{"APP_PAYMENT_RECONCILE_WINDOW": s("0s")}, "payment.reconcile_window"},
		"reconcile window, durable":              {[]string{"--require-durable"}, map[string]*string{"APP_PAYMENT_RECONCILE_WINDOW": s("0s")}, "payment.reconcile_window"},
		"payment window longer than the page":    {nil, map[string]*string{"APP_PAYMENT_MOCK_CHECKOUT_TTL": s("15m"), "APP_PAYMENT_WINDOW": s("16m")}, "payment.window"},
		"retention shorter than redelivery":      {nil, map[string]*string{"APP_PAYMENT_WEBHOOK_RETENTION": s("10h")}, "payment.webhook_retention"},
		"retention equal to redelivery":          {nil, map[string]*string{"APP_PAYMENT_WEBHOOK_RETENTION": s("72h")}, "payment.webhook_retention"},
		"negative retention":                     {nil, map[string]*string{"APP_PAYMENT_WEBHOOK_RETENTION": s("-1h")}, "payment.webhook_retention"},
		"no payment window":                      {nil, map[string]*string{"APP_PAYMENT_WINDOW": s("0s")}, "payment.window"},
		"no expire interval":                     {nil, map[string]*string{"APP_PAYMENT_EXPIRE_INTERVAL": s("0s")}, "payment.expire_interval"},
		"no expire batch":                        {nil, map[string]*string{"APP_PAYMENT_EXPIRE_BATCH": s("0")}, "payment.expire_batch"},
	} {
		t.Run(name, func(t *testing.T) {
			out := startAndStop(t, tc.args, durableEnv(pg.URL, tc.env), 3*time.Second)
			if out.served || out.err == nil || !strings.Contains(out.err.Error(), tc.want) {
				t.Fatalf("run(%q) = %+v, want a refusal naming %q", tc.args, out, tc.want)
			}
			for _, secret := range []string{pg.URL, pgPass, mockSecret, badArg} {
				if strings.Contains(out.err.Error(), secret) {
					t.Fatalf("the refusal prints %q: %v", secret, out.err)
				}
			}
			if strings.Contains(strings.Join(tc.args, " "), "require-durable") && strings.Contains(tc.want, "mock_webhook_secret") &&
				!strings.Contains(out.err.Error(), "--require-durable") {
				t.Fatalf("the refusal does not name --require-durable: %v", out.err)
			}
		})
	}
}

// Positive controls for the matrix above: the same environment with nothing wrong starts and stays up until
// told to stop, with and without the flag. One carries the provider credential (the mock's webhook secret) that
// apps/worker refuses, and none of the auth settings payment-rpc serve judges (the consumer listens on nothing),
// so neither makes it refuse.
func TestRun_Starts(t *testing.T) {
	pg, _ := workerDB(t)
	for name, args := range map[string][]string{"without the flag": nil, "with --require-durable": {"--require-durable"}} {
		t.Run(name, func(t *testing.T) {
			// No listener: the address http.addr names is held by this test, so a process that tried to serve on it
			// would fail with "address already in use" instead of running.
			held, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = held.Close() }()
			out := startAndStop(t, args, durableEnv(pg.URL, map[string]*string{
				"APP_AUTH_MODE": s("off"), "APP_AUTH_API_KEYS": s("console=x:admin,rpc=y:admin"), // serve's auth contract is no business of the consumer
				"APP_HTTP_ADDR": s(held.Addr().String()),
			}), 3*time.Second)
			if out.err != nil || !out.served {
				t.Fatalf("run(%q) = %+v, want it running until told to stop, then nil", args, out)
			}
		})
	}
}

// The consumer group is the offset cursor: it must stay worker.consumer_group + ".payment" or start_from=latest skips
// what was published while a rolling change swapped the workloads (#410). This observes the database, not a string:
// run() consumes one OrderConfirmed and the only cursor the offsets table holds is the one named "<group>.payment"
// (pkgs/payment.ConsumerGroup, which `payment-rpc consume` takes its name from too).
func TestRun_ConsumerGroupIsPinned(t *testing.T) {
	pg, db := workerDB(t)
	pub, err := eventbus.NewPostgresPublisher(db, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	orderID, msgID := "ord-pin", mustID(t)
	payload, err := proto.Marshal(&orderingv1.OrderConfirmed{OrderId: orderID, CustomerId: "cust-pin", Total: &commonv1.Money{AmountMinor: 1500, Currency: "TWD"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = pub.Publish(topicConf, message.NewMessage(msgID, payload)); err != nil {
		t.Fatal(err)
	}

	const group = "pinned-group"
	out := startAndStop(t, []string{"--require-durable"}, durableEnv(pg.URL, map[string]*string{"APP_WORKER_CONSUMER_GROUP": s(group)}), 3*time.Second)
	if out.err != nil || !out.served {
		t.Fatalf("run = %+v, want it running until told to stop, then nil", out)
	}

	var n int
	if err := db.Raw(`SELECT count(*) FROM payment.payments WHERE order_id = ?`, orderID).Scan(&n).Error; err != nil || n != 1 {
		t.Fatalf("payments of %s = %d (%v), want 1: the consumer did not handle the event", orderID, n, err)
	}
	var groups []string
	if err := db.Raw(fmt.Sprintf(`SELECT consumer_group FROM %s ORDER BY 1`, eventbus.OffsetsTableName(topicConf))).Scan(&groups).Error; err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0] != group+".payment" {
		t.Fatalf("consumer groups on %s = %q, want exactly [%q]", topicConf, groups, group+".payment")
	}
}

func mustID(t *testing.T) string {
	t.Helper()
	id, err := idgen.New()
	if err != nil {
		t.Fatal(err)
	}

	return id
}
