package main

import (
	"context"
	"strings"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

// docs/payment.md §8 (S6): ordering.require_payment on with no payment-rpc to pay at leaves every new order
// CONFIRMED with a snapshot nobody can fulfil, until the 24h reservation TTL cancels it. rpc already refuses
// to start in that state (payment client); the worker is where the snapshot is taken, so it refuses too, by
// the same rule, before it connects to anything. Only payment.upstream is required here, not the key: the
// key is rpc's service credential, and giving it to the worker would let a compromised worker call payment-rpc
// as rpc (docs/payment.md §1, the chart's credential isolation).
//
// The tests enter through the real command (newRootCmd -> RunE), not through a check function that NewWorker
// could stop calling. The database URL points at a closed port: a worker that gets past the guard fails
// connecting, with a different error, which is how the controls below tell "passed the guard" from "stopped
// by it".
func TestWorker_RequirePaymentNeedsPaymentUpstream(t *testing.T) {
	const closedPort = "postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=1"
	const guard = "ordering.require_payment"

	for _, tc := range []struct {
		name      string
		ordering  config.Ordering
		payment   config.Payment
		wantGuard bool
	}{
		{"switch on, payment.upstream unset", config.Ordering{RequirePayment: true}, config.Payment{}, true},
		{"switch on, only the key set", config.Ordering{RequirePayment: true}, config.Payment{APIKey: "k"}, true},
		{"switch on, upstream set", config.Ordering{RequirePayment: true}, config.Payment{Upstream: "http://x-monorepo-payment-rpc:80"}, false},
		{"switch off, nothing set (the default)", config.Ordering{}, config.Payment{}, false},
		{"switch off, upstream set", config.Ordering{}, config.Payment{Upstream: "http://x-monorepo-payment-rpc:80"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.AppConfig{Postgres: config.Postgres{URL: closedPort}, Ordering: tc.ordering, Payment: tc.payment}
			cmd := newRootCmd(func() (*config.AppConfig, error) { return cfg, nil })
			cmd.SetArgs(nil)
			c, cancel := context.WithTimeout(context.Background(), 30e9)
			defer cancel()

			err := cmd.ExecuteContext(c)
			if err == nil {
				t.Fatal("the worker ran to completion against a closed database port")
			}
			got := strings.Contains(err.Error(), guard)
			switch {
			case tc.wantGuard && !got:
				t.Fatalf("a worker with the switch on and no payment.upstream was not stopped by the guard: %v", err)
			case tc.wantGuard && !strings.Contains(err.Error(), "payment.upstream"):
				t.Fatalf("the refusal does not name the setting to fix: %v", err)
			case !tc.wantGuard && got:
				t.Fatalf("the guard stopped a configuration it must allow: %v", err)
			case !tc.wantGuard && !strings.Contains(err.Error(), "open postgres"):
				// the positive control: the failure is the connection, so the guard was passed
				t.Fatalf("expected the connection error that shows the guard was passed, got: %v", err)
			}
		})
	}
}
