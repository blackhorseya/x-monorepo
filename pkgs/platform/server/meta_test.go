package server

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/trace"
)

type msg struct{}

func callMeta(t *testing.T, c context.Context, inbound string, next connect.UnaryFunc) (connect.AnyResponse, error) {
	t.Helper()
	req := connect.NewRequest(&msg{})
	if inbound != "" {
		req.Header().Set(RequestIDHeader, inbound)
	}

	return NewMetaInterceptor()(next)(c, req)
}

func TestMeta_EchoesClientRequestIDAndExposesItToHandlers(t *testing.T) {
	var seen string
	res, err := callMeta(t, context.Background(), "client-42", func(c context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		seen = RequestID(c)
		return connect.NewResponse(&msg{}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != "client-42" || res.Header().Get(RequestIDHeader) != "client-42" {
		t.Fatalf("request id: handler saw %q, header %q", seen, res.Header().Get(RequestIDHeader))
	}
	if res.Header().Get(TraceIDHeader) != "" {
		t.Fatal("no span in context → no X-Trace-Id")
	}
}

func TestMeta_GeneratesRequestIDAndSetsItOnErrors(t *testing.T) {
	tid, _ := trace.TraceIDFromHex("4af8f2f9edd2565922620f5ef83e4626")
	sid, _ := trace.SpanIDFromHex("2a593bd5eebcae93")
	c := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled,
	}))

	for name, next := range map[string]connect.UnaryFunc{
		"connect error": func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("nope"))
		},
		"plain error": func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
			return nil, errors.New("boom")
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := callMeta(t, c, "", next)
			var ce *connect.Error
			if !errors.As(err, &ce) {
				t.Fatalf("want *connect.Error, got %T", err)
			}
			if ce.Meta().Get(RequestIDHeader) == "" {
				t.Fatal("generated request id missing on error")
			}
			if got := ce.Meta().Get(TraceIDHeader); got != tid.String() {
				t.Fatalf("X-Trace-Id = %q, want %s", got, tid)
			}
		})
	}
}
