package payment_test

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	paymentbc "github.com/blackhorseya/x-monorepo/pkgs/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

// The consumer group is the offset cursor of payment's consumer (docs/payment.md §1). Both entries
// (`payment-rpc consume` and `payment-worker`) take it from this one function: a new name would
// start every group from `worker.start_from` and, on `latest`, skip what was published meanwhile.
func TestConsumerGroup(t *testing.T) {
	for name, tc := range map[string]struct{ group, want string }{
		"the default group":   {"worker", "worker.payment"},
		"a group of its own":  {"staging", "staging.payment"},
		"a dotted group name": {"a.b", "a.b.payment"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := paymentbc.ConsumerGroup(config.Worker{ConsumerGroup: tc.group}); got != tc.want {
				t.Fatalf("ConsumerGroup(%q) = %q, want %q", tc.group, got, tc.want)
			}
		})
	}
}

// NewConsumer refuses what it cannot serve, and the refusal names the entry (the process the operator
// started) and the setting, never a value.
func TestNewConsumer_Refuses(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for name, tc := range map[string]struct {
		entry  string
		worker config.Worker
		want   string
	}{
		"no database (payment-worker)":      {"payment-worker", config.Worker{Broker: config.BrokerPostgres}, "postgres.url"},
		"no database (payment-rpc consume)": {"payment-rpc consume", config.Worker{Broker: config.BrokerPostgres}, "postgres.url"},
		"a redis broker":                    {"payment-worker", config.Worker{Broker: config.BrokerRedis}, "worker.broker"},
		"an unknown broker":                 {"payment-worker", config.Worker{Broker: "kafka"}, "worker.broker"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := paymentbc.NewConsumer(tc.entry, tc.worker, nil, logger, nil, nil, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), tc.entry) {
				t.Fatalf("NewConsumer = %v, want a refusal naming %q and %q", err, tc.entry, tc.want)
			}
		})
	}
}

// CheckDurableConsume is --require-durable for the consumer and the sweep: Postgres is the outbox it reads, and with the
// mock provider the webhook secret has to be a real value.
func TestCheckDurableConsume(t *testing.T) {
	good := func() *config.AppConfig {
		cfg := &config.AppConfig{}
		cfg.Postgres.URL = "postgres://u:p@h/db"
		cfg.Payment.Provider = paymentbc.MockProvider
		cfg.Payment.MockWebhookSecret = "a-shared-secret"
		return cfg
	}
	if err := paymentbc.CheckDurableConsume("payment-worker", good()); err != nil {
		t.Fatalf("a durable configuration was refused: %v", err)
	}
	for name, mutate := range map[string]func(*config.AppConfig){
		"postgres url empty":        func(c *config.AppConfig) { c.Postgres.URL = "" },
		"postgres url blank":        func(c *config.AppConfig) { c.Postgres.URL = "  \t" },
		"mock webhook secret empty": func(c *config.AppConfig) { c.Payment.MockWebhookSecret = "" },
		"mock webhook secret blank": func(c *config.AppConfig) { c.Payment.MockWebhookSecret = " \t\n" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := good()
			mutate(cfg)
			err := paymentbc.CheckDurableConsume("payment-worker", cfg)
			if err == nil || !strings.Contains(err.Error(), "require-durable") || !strings.Contains(err.Error(), "payment-worker") {
				t.Fatalf("CheckDurableConsume = %v, want a refusal naming --require-durable and payment-worker", err)
			}
			for _, secret := range []string{"postgres://u:p@h/db", "a-shared-secret"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("the refusal prints a secret: %v", err)
				}
			}
		})
	}
}
