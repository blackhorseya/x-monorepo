// Package contextx enriches context.Context with request-scoped helpers.
//
// Convention: interfaces take `c context.Context`; implementations convert once
// with `ctx := contextx.FromContext(c)` and use ctx for both cancellation and logging.
package contextx

import (
	"context"
	"log/slog"
)

type loggerKey struct{}

// Contextx is a context.Context carrying a request-scoped logger. Its logging
// methods pass the context along, so context-aware handlers (the OTel bridge)
// attach trace_id / span_id to every record automatically.
type Contextx struct {
	context.Context
	*slog.Logger
}

func (x Contextx) Debug(msg string, args ...any) { x.log(slog.LevelDebug, msg, args...) }
func (x Contextx) Info(msg string, args ...any)  { x.log(slog.LevelInfo, msg, args...) }
func (x Contextx) Warn(msg string, args ...any)  { x.log(slog.LevelWarn, msg, args...) }
func (x Contextx) Error(msg string, args ...any) { x.log(slog.LevelError, msg, args...) }

func (x Contextx) log(level slog.Level, msg string, args ...any) {
	x.Log(x.Context, level, msg, args...)
}

// FromContext wraps c. It never returns a nil logger.
func FromContext(c context.Context) Contextx {
	if c == nil {
		c = context.Background()
	}
	if ctx, ok := c.(Contextx); ok {
		return ctx
	}

	l, ok := c.Value(loggerKey{}).(*slog.Logger)
	if !ok || l == nil {
		l = slog.Default()
	}

	return Contextx{Context: c, Logger: l}
}

// WithLogger returns a child context carrying l.
func WithLogger(c context.Context, l *slog.Logger) context.Context {
	return context.WithValue(c, loggerKey{}, l)
}
