// Package config owns AppConfig. Mapping AppConfig into per-component configs
// happens only in each app's wire.go (wire.FieldsOf), nowhere else.
package config

import (
	"fmt"
	"os"
)

// AppConfig is the root configuration of a deployable.
type AppConfig struct {
	HTTP HTTP
	Log  Log
}

// HTTP configures the Connect/gRPC/HTTP server.
type HTTP struct {
	Addr string
	// Docs mounts /docs and /openapi/*.yaml. Keep false in production.
	Docs bool
}

// Log configures the structured logger.
type Log struct {
	Level  string // debug | info | warn | error
	Format string // json | text
}

// Load reads AppConfig from environment variables with sane defaults.
func Load() (*AppConfig, error) {
	cfg := &AppConfig{
		HTTP: HTTP{
			Addr: getenv("APP_HTTP_ADDR", ":8080"),
			Docs: getenv("APP_HTTP_DOCS", "false") == "true",
		},
		Log: Log{
			Level:  getenv("APP_LOG_LEVEL", "info"),
			Format: getenv("APP_LOG_FORMAT", "json"),
		},
	}

	if cfg.Log.Format != "json" && cfg.Log.Format != "text" {
		return nil, fmt.Errorf("invalid APP_LOG_FORMAT %q", cfg.Log.Format)
	}

	return cfg, nil
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}

	return def
}
