package server

import (
	"context"
	"errors"

	"connectrpc.com/connect/v2"
	"go.opentelemetry.io/otel/trace"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/idgen"
)

// Response metadata lives in headers, not in the proto messages: Connect, gRPC
// and gRPC-Web all carry them, generated clients stay flat, and the error path
// (x-error-code) already works this way.
const (
	// RequestIDHeader is echoed back when the client sends it, generated otherwise.
	RequestIDHeader = "X-Request-Id"
	// TraceIDHeader is the OpenTelemetry trace id — paste it into Tempo / Loki.
	TraceIDHeader = "X-Trace-Id"

	maxRequestIDLen = 128
)

type requestIDKey struct{}

// RequestID returns the request id the meta interceptor stored in c ("" outside an RPC).
func RequestID(c context.Context) string {
	id, _ := c.Value(requestIDKey{}).(string)
	return id
}

// NewMetaInterceptor assigns a request id (client-provided or a fresh UUID v7), keeps
// it in the context for logging, and sets X-Request-Id on every response and
// X-Trace-Id on every response that has a trace (none when otel.endpoint is
// empty) — success or error. Place it after otelconnect so the span exists.
//
// It is also where an error's metadata (WithMeta: x-error-code, Retry-After) becomes
// response metadata, and where a plain error becomes Internal: a v2 transport sends only
// a *connect.Error, anything else would reach the client as Unknown without a message.
// Where the metadata goes follows v1: headers for a Connect unary RPC, trailers for the
// other protocols (an error has no response headers of its own there).
func NewMetaInterceptor() connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(c context.Context, spec connect.Spec, stream connect.ServerStream) error {
			info := callInfo(c)
			rid := info.RequestHeader().Get(RequestIDHeader)
			if rid == "" || len(rid) > maxRequestIDLen {
				var err error
				if rid, err = idgen.New(); err != nil {
					// The system's randomness source failed. There is no v4 fallback:
					// every new id is v7, so fail the request as internal.
					return connect.NewError(connect.CodeInternal, "internal error")
				}
			}
			var tid string
			if sc := trace.SpanContextFromContext(c); sc.IsValid() {
				tid = sc.TraceID().String()
			}
			set := func(h *connect.Header) {
				h.Set(RequestIDHeader, rid)
				if tid != "" {
					h.Set(TraceIDHeader, tid)
				}
			}
			// A v2 handler's response headers are written when it sends its response
			// (unary: before the body), so they are set before next, not after it.
			set(info.ResponseHeader())

			err := next(context.WithValue(c, requestIDKey{}, rid), spec, stream)
			if err == nil {
				return nil
			}
			var ce *connect.Error
			if !errors.As(err, &ce) {
				ce = connect.NewError(connect.CodeInternal, "internal error")
			}
			meta := info.ResponseHeader()
			if !errorMetaInHeaders(info, spec) {
				// v1 answered an error on these protocols with trailers only.
				meta = info.ResponseTrailer()
				info.ResponseHeader().Delete(RequestIDHeader)
				info.ResponseHeader().Delete(TraceIDHeader)
				set(meta)
			}
			for k, vs := range ErrorMeta(ce) {
				meta.SetValues(k, vs)
			}

			return ce
		}
	}
}

// errorMetaInHeaders reports whether an error's metadata is sent as response
// headers: the Connect protocol's unary form, whose error is the HTTP response
// itself. Every other form (gRPC, gRPC-Web, Connect streaming) ends with trailers.
func errorMetaInHeaders(info *connect.CallInfo, spec connect.Spec) bool {
	return info.Protocol == "connect" && spec.StreamType == connect.StreamTypeUnary
}

// dropHeadersOnError removes the named response headers when the RPC fails. otelconnect
// v2 injects traceparent before the handler runs, so an error response would carry it;
// the v1 interceptor wrote it on a successful response only, and the docs promise the
// same (an error's trace is found with X-Trace-Id). Place it right inside otelconnect.
func dropHeadersOnError(names ...string) connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(c context.Context, spec connect.Spec, stream connect.ServerStream) error {
			err := next(c, spec, stream)
			if err != nil {
				info := callInfo(c)
				for _, n := range names {
					info.ResponseHeader().Delete(n)
				}
			}

			return err
		}
	}
}
