package config

import (
	"strings"
	"testing"
)

// ProviderCredentials names settings, never values, and counts a blank value as no credential (nothing is held).
func TestProviderCredentials(t *testing.T) {
	const secret = "mock-webhook-secret-0123456789"
	cases := []struct {
		name  string
		value string
		want  []string
	}{
		{"unset", "", nil},
		{"blank", "  \t\n", nil},
		{"set", secret, []string{"payment.mock_webhook_secret"}},
		{"set with spaces around", "  " + secret + " ", []string{"payment.mock_webhook_secret"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &AppConfig{Payment: Payment{MockWebhookSecret: tc.value}}
			got := cfg.ProviderCredentials()
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("ProviderCredentials() = %v, want %v", got, tc.want)
			}
			err := cfg.RejectProviderCredentials("worker")
			if (err != nil) != (len(tc.want) > 0) {
				t.Fatalf("RejectProviderCredentials() = %v, want an error iff a credential is set", err)
			}
			if err != nil && (strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "worker")) {
				t.Fatalf("the refusal prints the value or omits the process: %v", err)
			}
		})
	}
	// the settings rpc legitimately reads are not credentials of a provider
	cfg := &AppConfig{Payment: Payment{Upstream: "http://payment:80", APIKey: "k", Provider: "mock"}}
	if got := cfg.ProviderCredentials(); len(got) != 0 {
		t.Fatalf("rpc's own payment settings were taken for a provider credential: %v", got)
	}
}
