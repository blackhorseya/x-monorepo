package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

const exampleConfig = "../../configs/catalog-rpc.example.yaml"

// loadFile is `task catalog-rpc` as far as the config goes: APP_CONFIG points at path and the file
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

// taskfileVar reads a value `task run` takes from the Taskfile: the file and the Taskfile are two
// places that must agree, and this is the one test that says so.
func taskfileVar(t *testing.T, pattern string) string {
	t.Helper()
	raw, err := os.ReadFile("../../Taskfile.yml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(pattern).FindSubmatch(raw)
	if m == nil {
		t.Fatalf("Taskfile.yml has nothing matching %s", pattern)
	}

	return string(m[1])
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
	path := filepath.Join(t.TempDir(), "catalog-rpc.yaml")
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), "\nauth:\n", "\nauth:\n"+extra, 1)), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
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

	// `task run` and this file are two ends of one connection: the Taskfile's port and key are the file's.
	if want := ":" + taskfileVar(t, `CATALOG_PORT: '\{\{\.CATALOG_PORT \| default "(\d+)"\}\}'`); cfg.HTTP.Addr != want {
		t.Errorf("http.addr = %q, want %q (CATALOG_PORT of `task run`)", cfg.HTTP.Addr, want)
	}
	if want := "rpc=" + taskfileVar(t, `(?m)^  CATALOG_DEV_KEY: (\S+)$`) + ":admin"; cfg.Auth.APIKeys != want {
		t.Errorf("auth.api_keys = %q, want %q (CATALOG_DEV_KEY of `task run`)", cfg.Auth.APIKeys, want)
	}
	if cfg.Auth.Mode != config.AuthRequired {
		t.Errorf("auth.mode = %q, want %q", cfg.Auth.Mode, config.AuthRequired)
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
