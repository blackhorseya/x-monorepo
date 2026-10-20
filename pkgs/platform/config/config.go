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
}

// Auth configures the server's auth interceptor (platform/server.NewAuthInterceptor):
// who an RPC runs as, carried to use cases as contextx.Principal. It is
// authentication only; nothing here grants or denies anything.
type Auth struct {
	// Mode: "off" (no interceptor, every call is anonymous), "optional"
	// (a credential is verified when present, a call without one is
	// anonymous), "required" (no valid credential → Unauthenticated).
	Mode string `mapstructure:"mode"`
	// APIKeys are static keys as "name=key[,name=key…]" (one string so an env
	// var can carry it). A matching X-API-Key header makes the principal
	// {Subject: name, Method: "api_key"}.
	APIKeys string `mapstructure:"api_keys"`
	JWT     JWT    `mapstructure:"jwt"`
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
// reads it; an empty URL is fine with the default broker.
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
	v.SetDefault("auth.mode", AuthOff)
	v.SetDefault("auth.api_keys", "")
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

	return nil
}
