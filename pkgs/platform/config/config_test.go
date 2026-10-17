package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

// Run from a temp dir so no ./configs/config.yaml of the repo leaks in.
func chdirTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	for _, k := range []string{"APP_CONFIG", "APP_HTTP_ADDR", "APP_HTTP_DOCS", "APP_LOG_LEVEL", "APP_LOG_FORMAT", "APP_POSTGRES_URL"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	return dir
}

func TestLoad_DefaultsOnly(t *testing.T) {
	chdirTemp(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.Addr != ":8080" || cfg.HTTP.Docs || cfg.Log.Level != "info" || cfg.Log.Format != "json" || cfg.Postgres.URL != "" {
		t.Fatalf("defaults: %+v", cfg)
	}
}

func TestLoad_FileThenEnvOverrides(t *testing.T) {
	dir := chdirTemp(t)
	if err := os.MkdirAll(filepath.Join(dir, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := "http:\n  addr: \":9090\"\n  docs: true\nlog:\n  level: debug\n  format: text\npostgres:\n  url: postgres://file\n"
	if err := os.WriteFile(filepath.Join(dir, "configs", "config.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.Addr != ":9090" || !cfg.HTTP.Docs || cfg.Log.Level != "debug" || cfg.Log.Format != "text" || cfg.Postgres.URL != "postgres://file" {
		t.Fatalf("from file: %+v", cfg)
	}

	// env beats file; a key absent from the file still reads from env
	t.Setenv("APP_POSTGRES_URL", "postgres://env")
	t.Setenv("APP_HTTP_DOCS", "false")
	cfg, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Postgres.URL != "postgres://env" || cfg.HTTP.Docs || cfg.HTTP.Addr != ":9090" {
		t.Fatalf("env override: %+v", cfg)
	}
}

func TestLoad_ExplicitFile(t *testing.T) {
	dir := chdirTemp(t)
	path := filepath.Join(dir, "elsewhere.yaml")
	if err := os.WriteFile(path, []byte("http:\n  addr: \":7070\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APP_CONFIG", path)
	cfg, err := config.Load()
	if err != nil || cfg.HTTP.Addr != ":7070" {
		t.Fatalf("explicit file: %+v, %v", cfg, err)
	}

	t.Setenv("APP_CONFIG", filepath.Join(dir, "missing.yaml"))
	if _, err := config.Load(); err == nil {
		t.Fatal("explicit missing file must fail")
	}
}

func TestLoad_Validate(t *testing.T) {
	chdirTemp(t)
	t.Setenv("APP_LOG_FORMAT", "xml")
	if _, err := config.Load(); err == nil {
		t.Fatal("invalid log.format must fail")
	}
}
