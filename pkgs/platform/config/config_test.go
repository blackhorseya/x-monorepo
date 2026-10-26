package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if cfg.Auth.Mode != config.AuthOff || cfg.Auth.APIKeys != "" || len(cfg.Auth.Public) != 0 || cfg.Auth.AdminRole != "admin" {
		t.Fatalf("auth defaults: %+v", cfg.Auth)
	}
	if cfg.HTTP.ShutdownDelay != 0 || cfg.HTTP.ShutdownTimeout != 15*time.Second {
		t.Fatalf("shutdown defaults: %+v", cfg.HTTP)
	}
	if cfg.HTTP.Timeout != 10*time.Second || cfg.HTTP.RateLimit.RPS != 0 || cfg.HTTP.RateLimit.By != config.RateLimitGlobal {
		t.Fatalf("limit defaults: %+v", cfg.HTTP)
	}
	if p := cfg.Postgres; p.MaxOpenConns != 25 || p.MaxIdleConns != 10 || p.ConnMaxLifetime != 30*time.Minute || p.ConnMaxIdleTime != 5*time.Minute {
		t.Fatalf("postgres pool defaults: %+v", p)
	}
	if cfg.Worker.HandlerTimeout != 10*time.Second || cfg.Worker.PollInterval != 200*time.Millisecond {
		t.Fatalf("worker defaults: %+v", cfg.Worker)
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
	t.Setenv("APP_LOG_FORMAT", "json")

	t.Setenv("APP_HTTP_SHUTDOWN_DELAY", "-1s")
	if _, err := config.Load(); err == nil {
		t.Fatal("negative http.shutdown_delay must fail")
	}
	t.Setenv("APP_HTTP_SHUTDOWN_DELAY", "2s")
	t.Setenv("APP_HTTP_SHUTDOWN_TIMEOUT", "0")
	if _, err := config.Load(); err == nil {
		t.Fatal("zero http.shutdown_timeout must fail")
	}
	t.Setenv("APP_HTTP_SHUTDOWN_TIMEOUT", "500ms")
	t.Setenv("APP_HTTP_TIMEOUT", "-1s")
	if _, err := config.Load(); err == nil {
		t.Fatal("negative http.timeout must fail")
	}
	t.Setenv("APP_HTTP_TIMEOUT", "0")
	t.Setenv("APP_WORKER_HANDLER_TIMEOUT", "-1s")
	if _, err := config.Load(); err == nil {
		t.Fatal("negative worker.handler_timeout must fail")
	}
	t.Setenv("APP_WORKER_HANDLER_TIMEOUT", "0")
	t.Setenv("APP_WORKER_POLL_INTERVAL", "0")
	if _, err := config.Load(); err == nil {
		t.Fatal("zero worker.poll_interval must fail")
	}
	t.Setenv("APP_WORKER_POLL_INTERVAL", "-1s")
	if _, err := config.Load(); err == nil {
		t.Fatal("negative worker.poll_interval must fail")
	}
	t.Setenv("APP_WORKER_POLL_INTERVAL", "50ms")
	if cfg, err := config.Load(); err != nil || cfg.Worker.PollInterval != 50*time.Millisecond {
		t.Fatalf("worker.poll_interval override: %+v, %v", cfg.Worker, err)
	}
	t.Setenv("APP_WORKER_POLL_INTERVAL", "200ms")
	t.Setenv("APP_HTTP_RATE_LIMIT_RPS", "-1")
	if _, err := config.Load(); err == nil {
		t.Fatal("negative http.rate_limit.rps must fail")
	}
	t.Setenv("APP_HTTP_RATE_LIMIT_RPS", "2.5")
	t.Setenv("APP_HTTP_RATE_LIMIT_BY", "user")
	if _, err := config.Load(); err == nil {
		t.Fatal("unknown http.rate_limit.by must fail")
	}
	t.Setenv("APP_HTTP_RATE_LIMIT_BY", "peer")
	t.Setenv("APP_HTTP_RATE_LIMIT_BURST", "7")

	t.Setenv("APP_WORKER_BROKER", "kafka")
	if _, err := config.Load(); err == nil {
		t.Fatal("unknown worker.broker must fail")
	}
	t.Setenv("APP_WORKER_BROKER", "redis")
	if _, err := config.Load(); err == nil {
		t.Fatal("worker.broker=redis without redis.url must fail")
	}
	t.Setenv("APP_REDIS_URL", "redis://127.0.0.1:6379/0")
	t.Setenv("APP_WORKER_START_FROM", "yesterday")
	if _, err := config.Load(); err == nil {
		t.Fatal("unknown worker.start_from must fail")
	}
	t.Setenv("APP_WORKER_START_FROM", "latest")

	t.Setenv("APP_AUTH_MODE", "maybe")
	if _, err := config.Load(); err == nil {
		t.Fatal("unknown auth.mode must fail")
	}
	t.Setenv("APP_AUTH_MODE", "required")
	// Nothing static to verify against is allowed: the rpc server also resolves
	// keys through the identity BC (server.CredentialResolver); server.New warns.
	if _, err := config.Load(); err != nil {
		t.Fatalf("auth.mode=required with no static key / jwt must load (resolver-only): %v", err)
	}
	t.Setenv("APP_AUTH_RESOLVER_TIMEOUT", "-1s")
	if _, err := config.Load(); err == nil {
		t.Fatal("negative auth.resolver_timeout must fail")
	}
	t.Setenv("APP_AUTH_RESOLVER_TIMEOUT", "2s")
	t.Setenv("APP_AUTH_API_KEYS", "ci=secret")
	t.Setenv("APP_AUTH_JWT_HS256_SECRET", "s")
	t.Setenv("APP_AUTH_JWT_PUBLIC_KEY", "pem")
	if _, err := config.Load(); err == nil {
		t.Fatal("hs256_secret and public_key together must fail")
	}
	t.Setenv("APP_AUTH_JWT_PUBLIC_KEY", "")
	t.Setenv("APP_AUTH_PUBLIC", "/a.v1.S/Get,/a.v1.S/List")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Worker.Broker != config.BrokerRedis || cfg.Redis.URL == "" || cfg.Worker.StartFrom != config.StartFromLatest || cfg.Worker.HandlerTimeout != 0 {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.HTTP.ShutdownDelay != 2*time.Second || cfg.HTTP.ShutdownTimeout != 500*time.Millisecond {
		t.Fatalf("shutdown = %+v", cfg.HTTP)
	}
	if cfg.HTTP.Timeout != 0 || cfg.HTTP.RateLimit != (config.RateLimit{RPS: 2.5, Burst: 7, By: config.RateLimitPeer}) {
		t.Fatalf("limits = %+v", cfg.HTTP)
	}
	if cfg.Auth.Mode != config.AuthRequired || cfg.Auth.APIKeys != "ci=secret" || len(cfg.Auth.Public) != 2 || cfg.Auth.Public[1] != "/a.v1.S/List" {
		t.Fatalf("auth = %+v", cfg.Auth)
	}
}

// The example (what `task run` seeds configs/local.yaml from) must stay loadable.
func TestLoad_ExampleFile(t *testing.T) {
	example, err := filepath.Abs(filepath.Join("..", "..", "..", "configs", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	chdirTemp(t)
	t.Setenv("APP_CONFIG", example)

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HTTP.Docs || cfg.Log.Format != "text" || cfg.Postgres.URL == "" {
		t.Fatalf("unexpected example profile: %+v", cfg)
	}
}

func TestLoad_PostgresPool(t *testing.T) {
	chdirTemp(t)
	for _, tc := range []struct {
		name string
		env  map[string]string
		ok   bool
	}{
		{"override", map[string]string{"APP_POSTGRES_MAX_OPEN_CONNS": "40", "APP_POSTGRES_MAX_IDLE_CONNS": "40", "APP_POSTGRES_CONN_MAX_LIFETIME": "1h", "APP_POSTGRES_CONN_MAX_IDLE_TIME": "1m"}, true},
		{"unlimited open is an explicit opt-out", map[string]string{"APP_POSTGRES_MAX_OPEN_CONNS": "0"}, true},
		{"idle 0 means the database/sql default", map[string]string{"APP_POSTGRES_MAX_IDLE_CONNS": "0"}, true},
		{"never recycle", map[string]string{"APP_POSTGRES_CONN_MAX_LIFETIME": "0", "APP_POSTGRES_CONN_MAX_IDLE_TIME": "0"}, true},
		{"negative open", map[string]string{"APP_POSTGRES_MAX_OPEN_CONNS": "-1"}, false},
		{"negative idle", map[string]string{"APP_POSTGRES_MAX_IDLE_CONNS": "-1"}, false},
		{"idle above a capped open", map[string]string{"APP_POSTGRES_MAX_OPEN_CONNS": "5", "APP_POSTGRES_MAX_IDLE_CONNS": "6"}, false},
		{"negative lifetime", map[string]string{"APP_POSTGRES_CONN_MAX_LIFETIME": "-1s"}, false},
		{"negative idle time", map[string]string{"APP_POSTGRES_CONN_MAX_IDLE_TIME": "-1s"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cfg, err := config.Load()
			if (err == nil) != tc.ok {
				t.Fatalf("Load() = %+v, %v; want ok=%v", cfg.Postgres, err, tc.ok)
			}
			if tc.ok && tc.env["APP_POSTGRES_MAX_OPEN_CONNS"] == "40" && (cfg.Postgres.MaxOpenConns != 40 || cfg.Postgres.ConnMaxLifetime != time.Hour || cfg.Postgres.ConnMaxIdleTime != time.Minute) {
				t.Fatalf("override not applied: %+v", cfg.Postgres)
			}
		})
	}
}

// The negative-idle refusal tells the operator what 0 means, and 0 is not
// "no idle connections": NewDB skips the setter, so database/sql keeps its default.
func TestLoad_PostgresIdleMessageDescribesZero(t *testing.T) {
	chdirTemp(t)
	t.Setenv("APP_POSTGRES_MAX_IDLE_CONNS", "-1")
	_, err := config.Load()
	if err == nil {
		t.Fatal("negative postgres.max_idle_conns must fail")
	}
	if strings.Contains(err.Error(), "none idle") || !strings.Contains(err.Error(), "database/sql default") {
		t.Fatalf("error misdescribes max_idle_conns=0: %v", err)
	}
}
