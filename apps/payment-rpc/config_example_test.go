package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

const exampleConfig = "../../configs/payment-rpc.example.yaml"

// loadFile is `task payment-rpc` as far as the config goes: APP_CONFIG points at path and the file
// alone decides; every other APP_* of the test process is blanked.
func loadFile(t *testing.T, path string) (*config.AppConfig, error) {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, config.EnvPrefix+"_") {
			t.Setenv(k, "")
		}
	}
	t.Setenv(config.FileEnv, path)

	return config.Load()
}

// withAuth is the example with extra lines under its auth: section.
func withAuth(t *testing.T, extra string) string {
	t.Helper()
	raw, err := os.ReadFile(exampleConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\nauth:\n") {
		t.Fatal("the example has no auth: section")
	}
	path := filepath.Join(t.TempDir(), "payment-rpc.yaml")
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), "\nauth:\n", "\nauth:\n"+extra, 1)), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// The template `task payment-rpc` seeds is what a fresh clone runs on: through the real Load,
// NewAuth and CheckDurable it must be a configuration payment-rpc accepts, it must export
// telemetry (otel-lgtm is plaintext gRPC, so insecure; no service_name, or payment-rpc would
// report under rpc's name), and it must not carry the settings payment-rpc refuses.
func TestExampleConfig_IsWhatPaymentRPCRunsOn(t *testing.T) {
	cfg, err := loadFile(t, exampleConfig)
	if err != nil {
		t.Fatalf("Load(%s): %v", exampleConfig, err)
	}
	if _, err := NewAuth(cfg); err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	if err := CheckDurable(cfg); err != nil {
		t.Fatalf("CheckDurable: %v", err)
	}

	// the three services bind three ports on one machine
	if cfg.HTTP.Addr != ":8082" {
		t.Errorf("http.addr = %q, want :8082 (rpc is :8080, catalog-rpc :8081)", cfg.HTTP.Addr)
	}
	if want := "rpc=dev-payment-service-key-0123456789abcdef:admin"; cfg.Auth.APIKeys != want {
		t.Errorf("auth.api_keys = %q, want %q", cfg.Auth.APIKeys, want)
	}
	if cfg.Auth.Mode != config.AuthRequired {
		t.Errorf("auth.mode = %q, want %q", cfg.Auth.Mode, config.AuthRequired)
	}
	if cfg.OTel.Endpoint == "" {
		t.Error("otel.endpoint is empty: payment-rpc would not report to the local collector")
	}
	if !cfg.OTel.Insecure {
		t.Error("otel.insecure is false: otel-lgtm is plaintext gRPC")
	}
	if cfg.OTel.ServiceName != "" {
		t.Errorf("otel.service_name = %q, want empty (the executable name, payment-rpc)", cfg.OTel.ServiceName)
	}
	if cfg.Postgres.URL == "" {
		t.Error("postgres.url is empty")
	}
	// the payment section is what ProvidePaymentProvider accepts, with the reconcile window longer than the
	// mock's checkout pages (docs/payment.md §3)
	if _, err := payment.ProvidePaymentProvider(cfg.Payment, nil); err != nil {
		t.Errorf("payment section: %v", err)
	}
	if cfg.Payment.Provider != "mock" {
		t.Errorf("payment.provider = %q, want mock", cfg.Payment.Provider)
	}
	if cfg.Auth.JWT != (config.JWT{}) {
		t.Error("auth.jwt is set: payment-rpc refuses to start with it")
	}
	if cfg.Auth.OIDC.Enabled || len(cfg.Auth.OIDC.Issuers) > 0 {
		t.Error("auth.oidc is set: payment-rpc refuses to start with it")
	}
}

// A file that turns auth.oidc on is refused, and the message names the setting: with no issuer
// (config.Load's own validation) and with one (NewAuth: payment-rpc takes api_keys only).
func TestExampleConfig_OIDCInTheFileIsRefused(t *testing.T) {
	for name, extra := range map[string]string{
		"enabled, no issuer": "  oidc:\n    enabled: true\n",
		"enabled, one issuer": "  oidc:\n    enabled: true\n    issuers:\n" +
			"      - issuer: https://issuer.example\n        audience: console\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := loadFile(t, withAuth(t, extra))
			if err == nil {
				_, err = NewAuth(cfg)
			}
			if err == nil || !strings.Contains(err.Error(), "auth.oidc") {
				t.Fatalf("err = %v, want a refusal naming auth.oidc", err)
			}
		})
	}
}

// ...and so is auth.jwt in the file.
func TestExampleConfig_JWTInTheFileIsRefused(t *testing.T) {
	cfg, err := loadFile(t, withAuth(t, "  jwt:\n    issuer: https://issuer.example\n    hs256_secret: 0123456789abcdef0123456789abcdef\n"))
	if err == nil {
		_, err = NewAuth(cfg)
	}
	if err == nil || !strings.Contains(err.Error(), "auth.jwt") {
		t.Fatalf("err = %v, want a refusal naming auth.jwt", err)
	}
}

// Adding a section to the shared config must not break a file that never heard of it: every example
// under configs/ still loads, and the payment section it ends up with (its own, or the built-in
// defaults) is one payment-rpc would start on. A new required setting would show up here first.
func TestEveryExampleConfigStillLoads(t *testing.T) {
	files, err := filepath.Glob("../../configs/*.example.yaml")
	if err != nil || len(files) < 3 {
		t.Fatalf("glob = %v, %v; want at least config, catalog-rpc and payment-rpc examples", files, err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			cfg, err := loadFile(t, f)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if _, err := payment.ProvidePaymentProvider(cfg.Payment, nil); err != nil {
				t.Fatalf("payment section %+v: %v", cfg.Payment, err)
			}
		})
	}
}
