package otel

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/log"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

// newLogger builds the root slog.Logger: always stdout (format/level from cfg),
// plus the OTel bridge when lp is non-nil so every record also reaches Loki.
// Attributes become structured metadata there — query with `| key="v"`, not
// a line filter.
func newLogger(cfg config.Log, lp log.LoggerProvider) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}
	var stdout slog.Handler
	if cfg.Format == "text" {
		stdout = slog.NewTextHandler(os.Stdout, opts)
	} else {
		stdout = slog.NewJSONHandler(os.Stdout, opts)
	}

	if lp == nil {
		return slog.New(stdout)
	}

	otl := otelslog.NewHandler(scopeName, otelslog.WithLoggerProvider(lp))

	return slog.New(&fanout{level: level, handlers: []slog.Handler{stdout, otl}})
}

// fanout sends one record to every handler. One level gate applies to all of
// them, so Loki never gets records stdout would drop (or vice versa).
type fanout struct {
	level    slog.Leveler
	handlers []slog.Handler
}

func (f *fanout) Enabled(_ context.Context, l slog.Level) bool {
	return l >= f.level.Level()
}

func (f *fanout) Handle(c context.Context, r slog.Record) error {
	var errs []error
	for _, h := range f.handlers {
		errs = append(errs, h.Handle(c, r.Clone()))
	}

	return errors.Join(errs...)
}

func (f *fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	return f.map_(func(h slog.Handler) slog.Handler { return h.WithAttrs(attrs) })
}

func (f *fanout) WithGroup(name string) slog.Handler {
	if name == "" {
		return f
	}

	return f.map_(func(h slog.Handler) slog.Handler { return h.WithGroup(name) })
}

func (f *fanout) map_(fn func(slog.Handler) slog.Handler) *fanout {
	out := &fanout{level: f.level, handlers: make([]slog.Handler, len(f.handlers))}
	for i, h := range f.handlers {
		out.handlers[i] = fn(h)
	}

	return out
}
