// Package config owns AppConfig. Mapping AppConfig into per-component configs
// happens only in each app's wire.go (wire.FieldsOf), nowhere else.
//
// Sources, lowest to highest precedence:
//  1. defaults (below)
//  2. a YAML file: $APP_CONFIG if set (must exist), else the first of
//     ./configs/config.yaml, ./config.yaml (optional; see configs/config.example.yaml)
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
	Log      Log      `mapstructure:"log"`
	Postgres Postgres `mapstructure:"postgres"`
}

// HTTP configures the Connect/gRPC/HTTP server.
type HTTP struct {
	Addr string `mapstructure:"addr"`
	// Docs mounts /api/docs and /api/openapi/*.yaml. Keep false in production.
	Docs bool `mapstructure:"docs"`
}

// Postgres configures the shared connection pool. An empty URL means "no
// database": apps fall back to in-memory repositories (local dev, tests).
type Postgres struct {
	URL string `mapstructure:"url"` // e.g. postgres://user:pass@host:5432/db?sslmode=disable
}

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
	v.SetDefault("log.level", "info")
	v.SetDefault("log.format", "json")
	v.SetDefault("postgres.url", "")
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

	return nil
}
