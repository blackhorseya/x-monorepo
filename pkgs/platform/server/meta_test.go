package server

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect/v2"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
)

// callMeta runs the meta interceptor around next as connect.Server.Call does for one
// RPC (the Connect protocol's unary form unless protocol/streamType say otherwise),
// and returns the call's metadata next to the error.
func callMeta(t *testing.T, c context.Context, inbound string, next connect.ServerFunc) (*connect.CallInfo, error) {
	t.Helper()

	return callMetaOn(t, c, "connect", connect.StreamTypeUnary, inbound, next)
}

func callMetaOn(t *testing.T, c context.Context, protocol string, streamType connect.StreamType, inbound string, next connect.ServerFunc) (*connect.CallInfo, error) {
	t.Helper()
	spec := connect.Spec{Procedure: "/test.v1.TestService/Ping", StreamType: streamType}
	srv := connect.NewServer(NewMetaInterceptor())
	srv.Register(connect.Method{Spec: spec, Handler: next})
	info := &connect.CallInfo{Protocol: protocol}
	if inbound != "" {
		info.RequestHeader().Set(RequestIDHeader, inbound)
	}

	return info, srv.Call(c, spec.Procedure, info, nil)
}

func TestMeta_EchoesClientRequestIDAndExposesItToHandlers(t *testing.T) {
	var seen string
	info, err := callMeta(t, context.Background(), "client-42", func(c context.Context, _ connect.Spec, _ connect.ServerStream) error {
		seen = RequestID(c)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != "client-42" || info.ResponseHeader().Get(RequestIDHeader) != "client-42" {
		t.Fatalf("request id: handler saw %q, header %q", seen, info.ResponseHeader().Get(RequestIDHeader))
	}
	if info.ResponseHeader().Get(TraceIDHeader) != "" {
		t.Fatal("no span in context → no X-Trace-Id")
	}
}

func TestMeta_GeneratesRequestIDAndSetsItOnErrors(t *testing.T) {
	tid, _ := trace.TraceIDFromHex("4af8f2f9edd2565922620f5ef83e4626")
	sid, _ := trace.SpanIDFromHex("2a593bd5eebcae93")
	c := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled,
	}))

	for name, next := range map[string]connect.ServerFunc{
		"connect error": func(context.Context, connect.Spec, connect.ServerStream) error {
			return connect.NewError(connect.CodeNotFound, "nope")
		},
		"plain error": func(context.Context, connect.Spec, connect.ServerStream) error {
			return errors.New("boom")
		},
	} {
		t.Run(name, func(t *testing.T) {
			info, err := callMeta(t, c, "", next)
			var ce *connect.Error
			if !errors.As(err, &ce) {
				t.Fatalf("want *connect.Error, got %T", err)
			}
			rid := info.ResponseHeader().Get(RequestIDHeader)
			if rid == "" {
				t.Fatal("generated request id missing on error")
			}
			if u, err := uuid.Parse(rid); err != nil || u.Version() != 7 {
				t.Fatalf("generated request id %q: version %d, %v; want v7", rid, u.Version(), err)
			}
			if got := info.ResponseHeader().Get(TraceIDHeader); got != tid.String() {
				t.Fatalf("X-Trace-Id = %q, want %s", got, tid)
			}
		})
	}
}

// v1 sent an error's metadata (x-error-code, Retry-After, X-Request-Id) as response
// headers on a Connect unary RPC and as trailers on every other protocol. A plain error
// must reach the client as Internal with the same text, not as Unknown without a message.
func TestMeta_ErrorMetadataFollowsTheProtocol(t *testing.T) {
	failing := func(context.Context, connect.Spec, connect.ServerStream) error {
		return WithMeta(connect.NewError(connect.CodeResourceExhausted, "slow down"), "x-error-code", "request.rate_limited", RetryAfterHeader, "3")
	}
	for _, tc := range []struct {
		name       string
		protocol   string
		streamType connect.StreamType
		inHeaders  bool
	}{
		{"connect unary", "connect", connect.StreamTypeUnary, true},
		{"grpc", "grpc", connect.StreamTypeUnary, false},
		{"grpc-web", "grpcweb", connect.StreamTypeUnary, false},
		{"connect streaming", "connect", connect.StreamTypeServer, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, err := callMetaOn(t, context.Background(), tc.protocol, tc.streamType, "rid-1", failing)
			if connect.CodeOf(err) != connect.CodeResourceExhausted {
				t.Fatalf("err = %v", err)
			}
			where, other := info.ResponseTrailer(), info.ResponseHeader()
			if tc.inHeaders {
				where, other = other, where
			}
			for k, want := range map[string]string{"x-error-code": "request.rate_limited", RetryAfterHeader: "3", RequestIDHeader: "rid-1"} {
				if got := where.Get(k); got != want {
					t.Errorf("%s = %q, want %q", k, got, want)
				}
				if tc.inHeaders == false && other.Get(k) != "" {
					t.Errorf("%s also in the response headers: %q", k, other.Get(k))
				}
			}
		})
	}

	t.Run("plain error", func(t *testing.T) {
		_, err := callMeta(t, context.Background(), "", func(context.Context, connect.Spec, connect.ServerStream) error {
			return errors.New("dial tcp: secret host")
		})
		var ce *connect.Error
		if !errors.As(err, &ce) || ce.Code() != connect.CodeInternal || ce.Message() != "internal error" {
			t.Fatalf("err = %v, want Internal \"internal error\"", err)
		}
	})
}

func TestDropHeadersOnError(t *testing.T) {
	spec := connect.Spec{Procedure: "/test.v1.TestService/Ping", StreamType: connect.StreamTypeUnary}
	for name, err := range map[string]error{"success": nil, "failure": connect.NewError(connect.CodeNotFound, "nope")} {
		t.Run(name, func(t *testing.T) {
			srv := connect.NewServer(dropHeadersOnError("traceparent"))
			srv.Register(connect.Method{Spec: spec, Handler: func(c context.Context, _ connect.Spec, _ connect.ServerStream) error {
				callInfo(c).ResponseHeader().Set("Traceparent", "00-aa-bb-01")
				callInfo(c).ResponseHeader().Set("X-Other", "kept")
				return err
			}})
			info := &connect.CallInfo{}
			_ = srv.Call(context.Background(), spec.Procedure, info, nil)
			if got := info.ResponseHeader().Get("traceparent") != ""; got != (err == nil) {
				t.Fatalf("traceparent present = %v, want %v", got, err == nil)
			}
			if info.ResponseHeader().Get("X-Other") != "kept" {
				t.Fatal("other headers must stay")
			}
		})
	}
}
