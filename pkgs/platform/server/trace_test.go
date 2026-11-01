package server_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// pingRoute serves /test.v1.Echo/Ping, failing with a business error when the request asks for it.
type pingRoute struct{}

func (pingRoute) Route(opts ...connect.HandlerOption) (string, http.Handler) {
	return "/test.v1.Echo/", connect.NewUnaryHandler("/test.v1.Echo/Ping", func(_ context.Context, req *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
		if req.Header().Get("X-Fail") != "" {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("no such thing"))
		}

		return connect.NewResponse(&emptypb.Empty{}), nil
	}, opts...)
}

// A server built with a real SDK tracer and an in-memory exporter: a no-op tracer passes a parent's trace_id
// along unchanged, so only a real one can tell a continued trace from a new root.
func tracedServer(t *testing.T, opts ...server.Option) (*httptest.Server, *tracetest.InMemoryExporter) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tel := &otel.Providers{
		ServiceName:    "trace-test",
		Tracer:         sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)),
		Meter:          noop.NewMeterProvider(),
		Propagator:     propagation.TraceContext{},
		MetricsHandler: http.NotFoundHandler(),
	}
	srv, err := server.New(config.HTTP{Addr: ":0"}, config.Auth{}, slog.New(slog.NewTextHandler(io.Discard, nil)),
		[]server.Route{pingRoute{}}, nil, nil, tel, nil, server.OIDCAuth{}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return ts, exp
}

// ping calls Ping at base+path carrying a traceparent for remote, and returns the server span it produced and
// the X-Trace-Id of the response (a success's header or an error's metadata).
func ping(t *testing.T, ts *httptest.Server, exp *tracetest.InMemoryExporter, path string, remote trace.SpanContext, fail bool) (sdktrace.ReadOnlySpan, string) {
	t.Helper()
	if !remote.IsValid() || !remote.TraceID().IsValid() || !remote.SpanID().IsValid() {
		t.Fatal("the remote span context in the test is not valid")
	}
	exp.Reset()
	client := connect.NewClient[emptypb.Empty, emptypb.Empty](http.DefaultClient, ts.URL+path)
	req := connect.NewRequest(&emptypb.Empty{})
	propagation.TraceContext{}.Inject(trace.ContextWithRemoteSpanContext(context.Background(), remote), propagation.HeaderCarrier(req.Header()))
	if req.Header().Get("traceparent") == "" {
		t.Fatal("no traceparent was injected")
	}
	if fail {
		req.Header().Set("X-Fail", "1")
	}
	var header http.Header
	res, err := client.CallUnary(context.Background(), req)
	switch {
	case fail:
		var ce *connect.Error
		if !errors.As(err, &ce) || ce.Code() != connect.CodeNotFound {
			t.Fatalf("err = %v, want not_found", err)
		}
		header = ce.Meta()
	case err != nil:
		t.Fatal(err)
	default:
		header = res.Header()
	}

	var servers []sdktrace.ReadOnlySpan
	for _, s := range exp.GetSpans().Snapshots() {
		if s.SpanKind() == trace.SpanKindServer && s.Name() == "test.v1.Echo/Ping" {
			servers = append(servers, s)
		}
	}
	if len(servers) != 1 {
		t.Fatalf("got %d server spans for Ping, want 1: %v", len(servers), exp.GetSpans().Snapshots())
	}
	if !servers[0].SpanContext().IsValid() || !servers[0].SpanContext().TraceID().IsValid() {
		t.Fatal("the server span has no valid span context")
	}

	return servers[0], header.Get(server.TraceIDHeader)
}

func remoteSpan(t *testing.T) trace.SpanContext {
	t.Helper()
	tid, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	if err != nil {
		t.Fatal(err)
	}
	sid, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	if err != nil {
		t.Fatal(err)
	}

	return trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled, Remote: true})
}

// Both mounts of an RPC (the protocol path and the RPCPrefix one), success and business error alike, behave
// the same way for a given option.
var traceMounts = map[string]string{
	"protocol path": "/test.v1.Echo/Ping",
	"RPCPrefix":     server.RPCPrefix + "/test.v1.Echo/Ping",
}

func TestTrace_TrustRemoteContinuesTheCallersTrace(t *testing.T) {
	remote := remoteSpan(t)
	ts, exp := tracedServer(t, server.TrustRemoteTrace())
	for mount, path := range traceMounts {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fail=%v", mount, fail), func(t *testing.T) {
				span, header := ping(t, ts, exp, path, remote, fail)
				if span.Parent().TraceID() != remote.TraceID() || span.Parent().SpanID() != remote.SpanID() || !span.Parent().IsRemote() {
					t.Fatalf("server span parent = %v, want the remote span %v", span.Parent(), remote)
				}
				if span.SpanContext().TraceID() != remote.TraceID() {
					t.Fatalf("server span trace = %s, want the caller's %s", span.SpanContext().TraceID(), remote.TraceID())
				}
				if n := len(span.Links()); n != 0 {
					t.Fatalf("server span has %d links, want 0 (the remote span is its parent)", n)
				}
				if header != remote.TraceID().String() {
					t.Fatalf("X-Trace-Id = %q, want %s", header, remote.TraceID())
				}
			})
		}
	}
}

// Not trusting is what a server gets without saying anything, and what DistrustRemoteTrace says out loud.
func TestTrace_DistrustStartsANewRootAndLinksTheCaller(t *testing.T) {
	remote := remoteSpan(t)
	for name, opts := range map[string][]server.Option{
		"no option (the default)": nil,
		"DistrustRemoteTrace":     {server.DistrustRemoteTrace()},
		"Trust then Distrust":     {server.TrustRemoteTrace(), server.DistrustRemoteTrace()},
	} {
		ts, exp := tracedServer(t, opts...)
		for mount, path := range traceMounts {
			for _, fail := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/fail=%v", name, mount, fail), func(t *testing.T) {
					span, header := ping(t, ts, exp, path, remote, fail)
					if span.Parent().IsValid() {
						t.Fatalf("server span has parent %v, want a new root", span.Parent())
					}
					if span.SpanContext().TraceID() == remote.TraceID() {
						t.Fatalf("server span joined the caller's trace %s; it must start its own", remote.TraceID())
					}
					links := span.Links()
					if len(links) != 1 || links[0].SpanContext.TraceID() != remote.TraceID() || links[0].SpanContext.SpanID() != remote.SpanID() {
						t.Fatalf("links = %v, want exactly one pointing at the caller's span %v", links, remote)
					}
					if header != span.SpanContext().TraceID().String() || header == remote.TraceID().String() {
						t.Fatalf("X-Trace-Id = %q, want the server's own trace %s", header, span.SpanContext().TraceID())
					}
				})
			}
		}
	}
}
