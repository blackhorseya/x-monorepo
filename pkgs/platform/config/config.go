// Package config owns AppConfig. Mapping AppConfig into per-component configs
// happens only in each app's wire.go (wire.FieldsOf), nowhere else.
//
// Sources, lowest to highest precedence:
//  1. defaults (below)
//  2. a YAML file: $APP_CONFIG if set (must exist; `task run`/`task migrate` point it at
//     the gitignored configs/local.yaml, seeded from configs/config.example.yaml), else
//     the first of ./configs/config.yaml, ./config.yaml (optional)
//  3. environment: APP_<SECTION>_<KEY>, e.g. APP_HTTP_ADDR, APP_POSTGRES_URL
//
// Every key has a default so an env var alone is enough — no file, no flags.
package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// AppConfig is the root configuration of a deployable.
type AppConfig struct {
	HTTP     HTTP     `mapstructure:"http"`
	Auth     Auth     `mapstructure:"auth"`
	Log      Log      `mapstructure:"log"`
	Postgres Postgres `mapstructure:"postgres"`
	Redis    Redis    `mapstructure:"redis"`
	OTel     OTel     `mapstructure:"otel"`
	Worker   Worker   `mapstructure:"worker"`
}

// HTTP configures the Connect/gRPC/HTTP server.
type HTTP struct {
	Addr string `mapstructure:"addr"`
	// Docs mounts /api/docs and /api/openapi/*.yaml. Keep false in production.
	Docs bool `mapstructure:"docs"`
	// ShutdownDelay is how long the server keeps serving after SIGTERM with
	// /api/readyz already answering 503, so a load balancer that polls it
	// stops routing before the listener closes. 0 (default) closes at once;
	// behind Kubernetes set it to at least one readiness probe period.
	ShutdownDelay time.Duration `mapstructure:"shutdown_delay"`
	// ShutdownTimeout bounds the drain of in-flight requests after the
	// listener closed; what is still running then is cut off. Default 15s.
	// terminationGracePeriodSeconds must exceed shutdown_delay + shutdown_timeout.
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
	// Timeout is the server-side deadline of one RPC (platform/server
	// timeout interceptor): the use case's context expires and the client
	// gets DEADLINE_EXCEEDED. 0 = none. config.Load defaults it to 10s.
	Timeout time.Duration `mapstructure:"timeout"`
	// RateLimit is the token-bucket limit on RPCs (platform/server rate
	// limit interceptor). Off unless RPS is set.
	RateLimit RateLimit `mapstructure:"rate_limit"`
}

// RateLimit configures the rate limit interceptor: a token bucket refilled
// at RPS per second holding Burst tokens, either one bucket for the whole
// server (By "global") or one per client IP (By "peer", the connection's
// peer address — one key for everyone behind a proxy that does not preserve
// it). A request with no token gets RESOURCE_EXHAUSTED and Retry-After.
type RateLimit struct {
	RPS   float64 `mapstructure:"rps"`
	Burst int     `mapstructure:"burst"` // 0 = RPS rounded up
	By    string  `mapstructure:"by"`    // global | peer
}

// By values of RateLimit.By.
const (
	RateLimitGlobal = "global"
	RateLimitPeer   = "peer"
)

// Auth configures the server's auth interceptor (platform/server.NewAuthInterceptor):
// who an RPC runs as, carried to use cases as contextx.Principal. It is
// authentication only; nothing here grants or denies anything.
type Auth struct {
	// Mode: "off" (no interceptor, every call is anonymous), "optional"
	// (a credential is verified when present, a call without one is
	// anonymous), "required" (no valid credential → Unauthenticated).
	Mode string `mapstructure:"mode"`
	// APIKeys are static keys as "name=key[:role|role…][,name=key…]" (one
	// string so an env var can carry it). A matching X-API-Key header makes
	// the principal {Subject: name, Roles: roles, Method: "api_key"}.
	APIKeys string `mapstructure:"api_keys"`
	// AdminRole is the role admin.v1.AdminService requires when auth is on
	// (default "admin"); with auth.mode off it is not checked.
	AdminRole string `mapstructure:"admin_role"`
	JWT       JWT    `mapstructure:"jwt"`
	// Public lists procedures ("/<pkg>.<Service>/<Method>") that never need a
	// credential, whatever the mode.
	Public []string `mapstructure:"public"`
}

// JWT verifies `Authorization: Bearer <token>`: exactly one of HS256Secret
// (HS256) or PublicKey (PEM: RSA → RS256/384/512, EC → ES256/384/512, Ed25519 →
// EdDSA) must be set when auth is on and a JWT is expected. The token must
// carry exp, and iss / aud when Issuer / Audience are set. Claims read into
// the principal: sub, name, roles ([]string).
type JWT struct {
	Issuer      string `mapstructure:"issuer"`
	Audience    string `mapstructure:"audience"`
	HS256Secret string `mapstructure:"hs256_secret"`
	PublicKey   string `mapstructure:"public_key"`
}

// Mode values of Auth.Mode.
const (
	AuthOff      = "off"
	AuthOptional = "optional"
	AuthRequired = "required"
)

// Postgres configures the shared connection pool. An empty URL means "no
// database": apps fall back to in-memory repositories (local dev, tests).
type Postgres struct {
	URL string `mapstructure:"url"` // e.g. postgres://user:pass@host:5432/db?sslmode=disable
}

// Redis configures the Redis the worker uses as its broker when
// worker.broker is "redis" (platform/eventbus Redis Streams). Nothing else
// reads it, and with the default broker the worker does not even connect
// (apps/worker ProvideRedis), so a URL pointing at a Redis that is not
// running is harmless there.
type Redis struct {
	URL string `mapstructure:"url"` // e.g. redis://:pass@host:6379/0
}

// OTel configures OpenTelemetry export (traces + bridged Prometheus metrics)
// over OTLP/gRPC. An empty Endpoint disables it: providers become no-ops.
type OTel struct {
	Endpoint string `mapstructure:"endpoint"` // host:port, e.g. 127.0.0.1:4317
	Insecure bool   `mapstructure:"insecure"` // plaintext gRPC (local collectors)
	// ServiceName becomes service.name (Prometheus job); empty = executable name.
	ServiceName string `mapstructure:"service_name"`
}

// Worker configures apps/worker, the process that runs every BC's event
// consumers (platform/eventbus).
type Worker struct {
	// ConsumerGroup names the offset cursor: replicas sharing a group split
	// the work; a new group starts from the beginning of every topic.
	ConsumerGroup string `mapstructure:"consumer_group"`
	// Broker is where the handlers read from: "postgres" consumes the outbox
	// tables directly; "redis" has the worker forward each outbox table into a
	// Redis Stream and consume that (needs redis.url). The outbox stays the
	// write path either way.
	Broker string `mapstructure:"broker"`
	// StartFrom is where a consumer group that has NO cursor yet begins:
	// "beginning" replays every retained message, "latest" starts after what
	// is already there. A group that already has a cursor is never moved, so
	// this only matters the first time a group name (or the forwarder) appears.
	StartFrom string `mapstructure:"start_from"`
	// HandlerTimeout bounds one handler attempt (the consumer-side twin of
	// HTTP.Timeout): the use case's context expires, the attempt fails and is
	// retried, then parked on the poison topic. 0 = none.
	HandlerTimeout time.Duration `mapstructure:"handler_timeout"`
}

// Broker values of Worker.Broker.
const (
	BrokerPostgres = "postgres"
	BrokerRedis    = "redis"
)

// StartFrom values of Worker.StartFrom.
const (
	StartFromBeginning = "beginning"
	StartFromLatest    = "latest"
)

// Log configures the structured logger.
type Log struct {
	Level  string `mapstructure:"level"`  // debug | info | warn | error
	Format string `mapstructure:"format"` // json | text
}

// EnvPrefix is the prefix of every environment override (APP_HTTP_ADDR, ...).
const EnvPrefix = "APP"

// FileEnv names the env var that points at an explicit config file.
const FileEnv = EnvPrefix + "_CONFIG"

// Load builds AppConfig from defaults, an optional YAML file and APP_* env vars.
func Load() (*AppConfig, error) {
	v := viper.New()
	setDefaults(v)

	v.SetEnvPrefix(EnvPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if err := readFile(v); err != nil {
		return nil, err
	}

	var cfg AppConfig
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	return &cfg, cfg.validate()
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("http.addr", ":8080")
	v.SetDefault("http.docs", false)
	v.SetDefault("http.shutdown_delay", 0)
	v.SetDefault("http.shutdown_timeout", 15*time.Second)
	v.SetDefault("http.timeout", 10*time.Second)
	v.SetDefault("http.rate_limit.rps", 0)
	v.SetDefault("http.rate_limit.burst", 0)
	v.SetDefault("http.rate_limit.by", RateLimitGlobal)
	v.SetDefault("auth.mode", AuthOff)
	v.SetDefault("auth.api_keys", "")
	v.SetDefault("auth.admin_role", "admin")
	v.SetDefault("auth.jwt.issuer", "")
	v.SetDefault("auth.jwt.audience", "")
	v.SetDefault("auth.jwt.hs256_secret", "")
	v.SetDefault("auth.jwt.public_key", "")
	v.SetDefault("auth.public", []string{})
	v.SetDefault("log.level", "info")
	v.SetDefault("log.format", "json")
	v.SetDefault("postgres.url", "")
	v.SetDefault("redis.url", "")
	v.SetDefault("otel.endpoint", "")
	v.SetDefault("otel.insecure", false)
	v.SetDefault("otel.service_name", "")
	v.SetDefault("worker.consumer_group", "worker")
	v.SetDefault("worker.broker", BrokerPostgres)
	v.SetDefault("worker.start_from", StartFromBeginning)
	v.SetDefault("worker.handler_timeout", 10*time.Second)
}

func readFile(v *viper.Viper) error {
	v.SetConfigType("yaml")

	// APP_CONFIG is read through viper too, so it honours the same prefix rules.
	if explicit := v.GetString("config"); explicit != "" {
		v.SetConfigFile(explicit)
		if err := v.ReadInConfig(); err != nil {
			return fmt.Errorf("read %s=%s: %w", FileEnv, explicit, err)
		}
		return nil
	}

	v.SetConfigName("config")
	v.AddConfigPath("configs")
	v.AddConfigPath(".")
	err := v.ReadInConfig()
	var notFound viper.ConfigFileNotFoundError
	if err != nil && !errors.As(err, &notFound) {
		return fmt.Errorf("read config file: %w", err)
	}

	return nil
}

func (x *AppConfig) validate() error {
	if x.HTTP.ShutdownDelay < 0 {
		return fmt.Errorf("invalid http.shutdown_delay %s (want >= 0)", x.HTTP.ShutdownDelay)
	}
	if x.HTTP.ShutdownTimeout <= 0 {
		return fmt.Errorf("invalid http.shutdown_timeout %s (want > 0)", x.HTTP.ShutdownTimeout)
	}
	if x.HTTP.Timeout < 0 {
		return fmt.Errorf("invalid http.timeout %s (want >= 0, 0 = none)", x.HTTP.Timeout)
	}
	if x.HTTP.RateLimit.RPS < 0 || x.HTTP.RateLimit.Burst < 0 {
		return fmt.Errorf("invalid http.rate_limit rps=%v burst=%d (want >= 0)", x.HTTP.RateLimit.RPS, x.HTTP.RateLimit.Burst)
	}
	if by := x.HTTP.RateLimit.By; by != RateLimitGlobal && by != RateLimitPeer {
		return fmt.Errorf("invalid http.rate_limit.by %q (want global|peer)", by)
	}
	if x.Log.Format != "json" && x.Log.Format != "text" {
		return fmt.Errorf("invalid log.format %q (want json|text)", x.Log.Format)
	}
	switch x.Auth.Mode {
	case AuthOff:
	case AuthOptional, AuthRequired:
		if x.Auth.APIKeys == "" && x.Auth.JWT.HS256Secret == "" && x.Auth.JWT.PublicKey == "" {
			return fmt.Errorf("auth.mode is %s but no auth.api_keys, auth.jwt.hs256_secret or auth.jwt.public_key is set", x.Auth.Mode)
		}
	default:
		return fmt.Errorf("invalid auth.mode %q (want off|optional|required)", x.Auth.Mode)
	}
	if x.Auth.AdminRole == "" {
		return errors.New("auth.admin_role must not be empty")
	}
	if x.Auth.JWT.HS256Secret != "" && x.Auth.JWT.PublicKey != "" {
		return errors.New("auth.jwt.hs256_secret and auth.jwt.public_key are exclusive")
	}
	switch x.Worker.Broker {
	case BrokerPostgres:
	case BrokerRedis:
		if x.Redis.URL == "" {
			return errors.New("worker.broker is redis but redis.url is empty")
		}
	default:
		return fmt.Errorf("invalid worker.broker %q (want postgres|redis)", x.Worker.Broker)
	}
	if x.Worker.StartFrom != StartFromBeginning && x.Worker.StartFrom != StartFromLatest {
		return fmt.Errorf("invalid worker.start_from %q (want beginning|latest)", x.Worker.StartFrom)
	}
	if x.Worker.HandlerTimeout < 0 {
		return fmt.Errorf("invalid worker.handler_timeout %s (want >= 0, 0 = none)", x.Worker.HandlerTimeout)
	}

	return nil
}
