package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"go.opentelemetry.io/otel/trace"

	identityv1 "github.com/blackhorseya/x-monorepo/pkgs/identity/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/contract/v1/identityv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// deadCollector is an OTLP endpoint nothing listens on: rpc runs the real SDK (a no-op tracer hands a
// parent's trace_id on unchanged, which is how an older version of this test stayed green with the trace
// broken), and the exporter just fails in the background. Its Shutdown is bounded by the otel package's own 5s.
func deadCollector(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	return addr
}

const externalTrace = "4bf92f3577b34da6a3ce929d0e0e4736"

// An external caller's traceparent is never continued at rpc: with the SDK on (otel.endpoint set) the server
// span is a new root and rpc answers with its own X-Trace-Id; with it off (empty: no-op tracer) rpc neither
// reads nor writes trace headers at all. rpc has no downstream call today, so there is no "forwards the trace"
// cell; the first downstream call brings that cell back.
// The comparisons are against values asserted valid (or asserted absent) first, never empty against empty.
func TestTrace_RPCDistrustsAnExternalTraceparent(t *testing.T) {
	ext, err := trace.TraceIDFromHex(externalTrace)
	if err != nil || !ext.IsValid() {
		t.Fatal("the external trace id in the test is not valid")
	}
	for _, sdkOn := range []bool{true, false} {
		name := "sdk off"
		if sdkOn {
			name = "sdk on"
		}
		t.Run(name, func(t *testing.T) {
			cfg := &config.AppConfig{
				HTTP: config.HTTP{Addr: ":0"},
				Log:  config.Log{Level: "error", Format: "text"},
			}
			if sdkOn {
				cfg.OTel = config.OTel{Endpoint: deadCollector(t), Insecure: true, ServiceName: "rpc"}
			}
			if (cfg.OTel.Endpoint != "") != sdkOn {
				t.Fatalf("rpc otel.endpoint = %q, want it set = %v", cfg.OTel.Endpoint, sdkOn)
			}
			srv, cleanup, err := Initialize(cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(cleanup)
			ts := httptest.NewServer(srv.Handler())
			t.Cleanup(ts.Close)

			client := identityv1connect.NewIdentityServiceClient(connect.NewClient(connecthttp.NewTransport(http.DefaultClient, ts.URL, connecthttp.WithReadMaxBytes(0))))
			c, info := connect.NewClientContext(context.Background())
			info.RequestHeader().Set("traceparent", "00-"+externalTrace+"-00f067aa0ba902b7-01")
			if _, err := client.CreateAccount(c, &identityv1.CreateAccountRequest{Name: "tracer"}); err != nil {
				t.Fatalf("CreateAccount error = %v", err)
			}
			h := http.Header{}
			for k, v := range info.ResponseHeader().All() {
				h[k] = v
			}

			if sdkOn {
				if got := traceOf(t, h); got == ext {
					t.Fatalf("rpc continued the external caller's trace %s; it must start its own", ext)
				}

				return
			}
			for k, v := range h {
				if strings.EqualFold(k, server.TraceIDHeader) || strings.EqualFold(k, "traceparent") {
					t.Fatalf("rpc (no SDK) answered with %s = %v; it has no trace to name", k, v)
				}
			}
		})
	}
}

// traceOf is the X-Trace-Id of a response; it must be a valid, non-zero trace id.
func traceOf(t *testing.T, h http.Header) trace.TraceID {
	t.Helper()
	id, err := trace.TraceIDFromHex(h.Get(server.TraceIDHeader))
	if err != nil || !id.IsValid() {
		t.Fatalf("X-Trace-Id = %q, want a valid trace id (%v)", h.Get(server.TraceIDHeader), err)
	}

	return id
}
