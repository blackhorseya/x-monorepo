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

// Contextx is a context.Context carrying a request-scoped logger.
type Contextx struct {
	context.Context
	*slog.Logger
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
