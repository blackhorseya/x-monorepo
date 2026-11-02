package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

// The worker does not load payment, so it never holds a provider credential (docs/payment.md §1). The chart keeps
// payment-rpc's credential in payment-rpc's own Secret, but the worker reads the release-wide ConfigMap and Secret, and
// both can be written by an operator: config.payment.mock_webhook_secret in the values, APP_PAYMENT_MOCK_WEBHOOK_SECRET
// in secrets.existingSecret or worker.extraEnv. Whatever the way in, a worker whose FINAL loaded config carries one
// refuses to start, in every command (the retention and TTL CronJobs mount the same ConfigMap and Secret).
const providerCredential = "mock-webhook-secret-0123456789"

func TestWorker_RefusesAProviderCredential(t *testing.T) {
	file := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}

		return p
	}
	ways := map[string]func(t *testing.T){
		"env APP_PAYMENT_MOCK_WEBHOOK_SECRET (extraEnv / existingSecret / envFrom)": func(t *testing.T) {
			t.Setenv("APP_PAYMENT_MOCK_WEBHOOK_SECRET", providerCredential)
		},
		"config file payment.mock_webhook_secret (config.payment.* in the chart's values)": func(t *testing.T) {
			t.Setenv("APP_CONFIG", file(t, "payment:\n  mock_webhook_secret: "+providerCredential+"\n"))
		},
	}
	commands := map[string][]string{
		"consume (no subcommand)": {},
		"prune":                   {"prune", "--older-than", "720h", "--dry-run"},
		"expire":                  {"expire", "--older-than", "24h", "--dry-run"},
		"poison list":             {"poison", "list"},
	}
	for way, set := range ways {
		for name, args := range commands {
			t.Run(name+" via "+way, func(t *testing.T) {
				t.Setenv("APP_POSTGRES_URL", "") // nothing to connect to: the refusal has to come before any of it
				set(t)
				cmd := newRootCmd(config.Load)
				cmd.SetArgs(args)
				err := cmd.Execute()
				if err == nil || !strings.Contains(err.Error(), "payment.mock_webhook_secret") {
					t.Fatalf("worker %v = %v, want a refusal naming payment.mock_webhook_secret", args, err)
				}
				if strings.Contains(err.Error(), providerCredential) {
					t.Fatalf("the refusal prints the credential: %v", err)
				}
			})
		}
	}

	// positive control: the same commands without the credential are not refused for it (they fail later, on the missing
	// database), and the payment settings rpc legitimately reads from the shared config stay usable.
	t.Run("without the credential nothing is refused for it", func(t *testing.T) {
		t.Setenv("APP_POSTGRES_URL", "")
		t.Setenv("APP_CONFIG", file(t, "payment:\n  upstream: http://payment:80\n  api_key: x\n  timeout: 3s\n  provider: mock\n  window: 30m\n"))
		for _, args := range commands {
			cmd := newRootCmd(config.Load)
			cmd.SetArgs(args)
			if err := cmd.Execute(); err != nil && strings.Contains(err.Error(), "mock_webhook_secret") {
				t.Fatalf("worker %v refused for a credential it does not hold: %v", args, err)
			}
		}
	})
}
