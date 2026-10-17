package server

import (
	"context"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/trace"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// NewLoggingInterceptor injects a per-request logger and logs transport-level
// facts (procedure, protocol, code, trace_id when a span is active). Use-case-level
// logging is a usecasex decorator.
func NewLoggingInterceptor(logger *slog.Logger) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(c context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			l := logger.With("procedure", req.Spec().Procedure, "protocol", req.Peer().Protocol)
			if sc := trace.SpanContextFromContext(c); sc.IsValid() {
				l = l.With("trace_id", sc.TraceID().String())
			}
			ctx := contextx.FromContext(contextx.WithLogger(c, l))

			start := time.Now()
			res, err := next(ctx, req)

			code := "ok"
			if err != nil {
				code = connect.CodeOf(err).String()
			}
			ctx.Info("rpc", "code", code, "duration_ms", time.Since(start).Milliseconds())

			return res, err
		}
	}
}
