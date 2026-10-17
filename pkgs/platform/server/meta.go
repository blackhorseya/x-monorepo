package server

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
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

// NewMetaInterceptor assigns a request id (client-provided or fresh UUID), keeps
// it in the context for logging, and sets X-Request-Id / X-Trace-Id on every
// response — success or error. Place it after otelconnect so the span exists.
func NewMetaInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(c context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			rid := req.Header().Get(RequestIDHeader)
			if rid == "" || len(rid) > maxRequestIDLen {
				rid = uuid.NewString()
			}
			var tid string
			if sc := trace.SpanContextFromContext(c); sc.IsValid() {
				tid = sc.TraceID().String()
			}
			set := func(h http.Header) {
				h.Set(RequestIDHeader, rid)
				if tid != "" {
					h.Set(TraceIDHeader, tid)
				}
			}

			res, err := next(context.WithValue(c, requestIDKey{}, rid), req)
			if err != nil {
				var ce *connect.Error
				if !errors.As(err, &ce) {
					ce = connect.NewError(connect.CodeInternal, errors.New("internal error"))
				}
				set(ce.Meta())

				return nil, ce
			}
			set(res.Header())

			return res, nil
		}
	}
}
