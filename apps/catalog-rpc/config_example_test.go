package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

const exampleConfig = "../../configs/catalog-rpc.example.yaml"

// loadFile is `task catalog-rpc` as far as the config goes: APP_CONFIG points at path, and the
// Taskfile's env (mode, the one dev key) sits on top of it; every other APP_* of the test
// process is blanked so nothing but the file and those two decide.
func loadFile(t *testing.T, path string) (*config.AppConfig, error) {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, config.EnvPrefix+"_") {
			t.Setenv(k, "")
		}
	}
	t.Setenv(config.FileEnv, path)
	t.Setenv("APP_AUTH_MODE", "required")
	t.Setenv("APP_AUTH_API_KEYS", "rpc="+durableKey+":admin")

	return config.Load()
}

// The template `task catalog-rpc` seeds is what a fresh clone runs on: through the real Load,
// NewAuth and CheckDurable it must be a configuration catalog-rpc accepts, it must export
// telemetry (otel-lgtm is plaintext gRPC, so insecure; no service_name, or catalog-rpc would
// report under rpc's name), and it must not carry the settings catalog-rpc refuses.
func TestExampleConfig_IsWhatCatalogRPCRunsOn(t *testing.T) {
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

	if cfg.OTel.Endpoint == "" {
		t.Error("otel.endpoint is empty: catalog-rpc would not report to the local collector")
	}
	if !cfg.OTel.Insecure {
		t.Error("otel.insecure is false: otel-lgtm is plaintext gRPC")
	}
	if cfg.OTel.ServiceName != "" {
		t.Errorf("otel.service_name = %q, want empty (the executable name, catalog-rpc)", cfg.OTel.ServiceName)
	}
	if cfg.Postgres.URL == "" {
		t.Error("postgres.url is empty")
	}
	if cfg.Auth.JWT != (config.JWT{}) {
		t.Error("auth.jwt is set: catalog-rpc refuses to start with it")
	}
	if cfg.Auth.OIDC.Enabled || len(cfg.Auth.OIDC.Issuers) > 0 {
		t.Error("auth.oidc is set: catalog-rpc refuses to start with it")
	}
}

// A file that turns auth.oidc on is refused, and the message names the setting: with no issuer
// (config.Load's own validation) and with one (NewAuth: catalog-rpc takes api_keys only).
func TestExampleConfig_OIDCInTheFileIsRefused(t *testing.T) {
	raw, err := os.ReadFile(exampleConfig)
	if err != nil {
		t.Fatal(err)
	}
	for name, extra := range map[string]string{
		"enabled, no issuer": "\nauth:\n  oidc:\n    enabled: true\n",
		"enabled, one issuer": "\nauth:\n  oidc:\n    enabled: true\n    issuers:\n" +
			"      - issuer: https://issuer.example\n        audience: console\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "catalog-rpc.yaml")
			if err := os.WriteFile(path, append(append([]byte{}, raw...), extra...), 0o600); err != nil {
				t.Fatal(err)
			}

			cfg, err := loadFile(t, path)
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
	raw, err := os.ReadFile(exampleConfig)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "catalog-rpc.yaml")
	extra := "\nauth:\n  jwt:\n    issuer: https://issuer.example\n    hs256_secret: 0123456789abcdef0123456789abcdef\n"
	if err := os.WriteFile(path, append(append([]byte{}, raw...), extra...), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadFile(t, path)
	if err == nil {
		_, err = NewAuth(cfg)
	}
	if err == nil || !strings.Contains(err.Error(), "auth.jwt") {
		t.Fatalf("err = %v, want a refusal naming auth.jwt", err)
	}
}
