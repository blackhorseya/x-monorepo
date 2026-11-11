package server

import (
	"context"
	"log/slog"
	"time"

	"connectrpc.com/connect/v2"
	"go.opentelemetry.io/otel/trace"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// Auth values of the `auth` field on the rpc access line: how the caller was
// established. MethodAPIKey / MethodJWT (auth.go) are the authenticated ones.
const (
	// AuthAnonymous: no credential (auth.mode off or optional), or the
	// credential was rejected (the line's code says so).
	AuthAnonymous = "anonymous"
	// AuthPublic: the procedure is in auth.public, nothing was checked.
	AuthPublic = "public"
)

// access is the mutable slot the logging interceptor leaves in the context
// so an inner interceptor (auth) can report back what it established: a
// context value only flows inward, the access line is written on the way out.
type access struct {
	auth string
	sub  string
}

type accessKey struct{}

// recordAuth notes on the access line how the caller was established and,
// when authenticated, who. No-op outside the logging interceptor.
func recordAuth(c context.Context, method, sub string) {
	if a, ok := c.Value(accessKey{}).(*access); ok {
		a.auth, a.sub = method, sub
	}
}

// NewLoggingInterceptor injects a per-request logger and logs transport-level
// facts (procedure, protocol, code, request_id, trace_id when a span is active)
// plus who called: `auth` (api_key / jwt / anonymous / public) and, when a
// credential was verified, `sub` — the audit trail of every RPC, including
// the rejected ones (code unauthenticated, auth anonymous). Use-case-level
// logging is a usecasex decorator.
func NewLoggingInterceptor(logger *slog.Logger) connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(c context.Context, spec connect.Spec, stream connect.ServerStream) error {
			l := logger.With("procedure", spec.Procedure, "protocol", callInfo(c).Protocol)
			if rid := RequestID(c); rid != "" {
				l = l.With("request_id", rid)
			}
			if sc := trace.SpanContextFromContext(c); sc.IsValid() {
				l = l.With("trace_id", sc.TraceID().String())
			}
			a := &access{auth: AuthAnonymous}
			ctx := contextx.FromContext(contextx.WithLogger(context.WithValue(c, accessKey{}, a), l))

			start := time.Now()
			err := next(ctx, spec, stream)

			code := "ok"
			if err != nil {
				code = connect.CodeOf(err).String()
			}
			attrs := []any{"code", code, "duration_ms", time.Since(start).Milliseconds(), "auth", a.auth}
			if a.sub != "" {
				attrs = append(attrs, "sub", a.sub)
			}
			ctx.Info("rpc", attrs...)

			return err
		}
	}
}

// callInfo is the per-call metadata the transport put on c (headers, peer,
// protocol). A c that did not come through connect.Server.Call (a unit test
// running one interceptor on its own) gets an empty one, so callers need no nil check.
func callInfo(c context.Context) *connect.CallInfo {
	if info, ok := connect.CallInfoForServerContext(c); ok {
		return info
	}

	return &connect.CallInfo{}
}
