package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/trace"

	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1/orderingv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// deadCollector is an OTLP endpoint nothing listens on: both processes run the real SDK (a no-op tracer hands a
// parent's trace_id on unchanged, which is how the old version of this test stayed green with the trace broken),
// and the exporters just fail in the background. Their Shutdown is bounded by the otel package's own 5s.
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

// tracedPair is an rpc and a catalog-rpc, each with the OTel SDK on (otel.endpoint set) or off (empty: no-op
// tracer). The SDK state is read back from the processes themselves where it can be, so a cell that quietly ran
// in the other state fails here instead of passing vacuously.
type tracedPair struct {
	rpcURL string
	cat    *catalogProc
}

func startTracedPair(t *testing.T, rpcOn, catOn bool) tracedPair {
	t.Helper()
	endpoint := deadCollector(t)
	var catEnv []string
	if catOn {
		catEnv = []string{"APP_OTEL_ENDPOINT=" + endpoint, "APP_OTEL_INSECURE=true", "APP_OTEL_SERVICE_NAME=catalog-rpc"}
	}
	cat := startCatalogProc(t, catEnv...)
	if got := strings.Contains(cat.logs.String(), `"otel exporting"`); got != catOn {
		t.Fatalf("catalog-rpc SDK on = %v, want %v:\n%s", got, catOn, cat.logs.String())
	}
	cfg := withCatalog(&config.AppConfig{}, cat)
	if rpcOn {
		cfg.OTel = config.OTel{Endpoint: endpoint, Insecure: true, ServiceName: "rpc"}
	}
	if (cfg.OTel.Endpoint != "") != rpcOn {
		t.Fatalf("rpc otel.endpoint = %q, want it set = %v", cfg.OTel.Endpoint, rpcOn)
	}
	ts := rpcServer(t, cfg)
	seedProduct(t, ts.URL, "", "TR-1", 100, "USD")

	return tracedPair{rpcURL: ts.URL, cat: cat}
}

// quotePricesLines are the catalog's access lines for QuotePrices, as of now.
func quotePricesLines(cat *catalogProc) []map[string]any {
	var lines []map[string]any
	for _, l := range strings.Split(cat.logs.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["msg"] == "rpc" && strings.HasSuffix(fmt.Sprint(m["procedure"]), "/QuotePrices") {
			lines = append(lines, m)
		}
	}

	return lines
}

// nextQuotePrices waits (the child's stdout is copied by a goroutine) for the QuotePrices access line the call
// just made: the one after the first `before`.
func nextQuotePrices(t *testing.T, cat *catalogProc, before int) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if lines := quotePricesLines(cat); len(lines) > before {
			return lines[before]
		}
		if time.Now().After(deadline) {
			t.Fatalf("catalog-rpc logged no new QuotePrices line:\n%s", cat.logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

const externalTrace = "4bf92f3577b34da6a3ce929d0e0e4736"

// The trace across rpc and catalog-rpc for each state of their SDKs. An external caller's traceparent is never
// continued at rpc: with the SDK on it becomes a link of a new root, with it off rpc neither reads nor writes
// trace headers at all. catalog-rpc continues what rpc sends it (when it has an SDK to continue it with).
// The comparisons are against values asserted valid (or asserted absent) first, never empty against empty.
func TestTrace_RPCAndCatalogBySDKState(t *testing.T) {
	ext, err := trace.TraceIDFromHex(externalTrace)
	if err != nil || !ext.IsValid() {
		t.Fatal("the external trace id in the test is not valid")
	}
	for _, tc := range []struct {
		name          string
		rpcOn, catOn  bool
		wantRPCTrace  bool // rpc answers with an X-Trace-Id
		wantCatTrace  bool // the catalog's access line carries a trace_id
		wantSameTrace bool // ... and it is rpc's
	}{
		{"rpc on, catalog on", true, true, true, true, true},
		{"rpc off, catalog on", false, true, false, true, false},
		{"rpc on, catalog off", true, false, true, false, false},
		{"rpc off, catalog off", false, false, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pair := startTracedPair(t, tc.rpcOn, tc.catOn)
			client := orderingv1connect.NewOrderServiceClient(http.DefaultClient, pair.rpcURL)
			for path, items := range map[string][]*orderingv1.CreateOrderItem{
				"success":                                {{Sku: "TR-1", Quantity: 1}},
				"catalog's business error (unknown sku)": {{Sku: "NOPE", Quantity: 1}},
			} {
				before := len(quotePricesLines(pair.cat))
				req := connect.NewRequest(&orderingv1.CreateOrderRequest{CustomerId: "c-tr", Items: items})
				req.Header().Set("traceparent", "00-"+externalTrace+"-00f067aa0ba902b7-01")
				res, err := client.CreateOrder(context.Background(), req)
				var h http.Header
				if err != nil {
					var ce *connect.Error
					if !errors.As(err, &ce) {
						t.Fatal(err)
					}
					h = ce.Meta()
				} else {
					h = res.Header()
				}
				if (path == "success") != (err == nil) {
					t.Fatalf("%s: CreateOrder error = %v", path, err)
				}

				// rpc's own answer
				if tc.wantRPCTrace {
					if got := traceOf(t, h); got == ext {
						t.Fatalf("%s: rpc continued the external caller's trace %s; it must start its own", path, ext)
					}
				} else {
					for k, v := range h {
						if strings.EqualFold(k, server.TraceIDHeader) || strings.EqualFold(k, "traceparent") {
							t.Fatalf("%s: rpc (no SDK) answered with %s = %v; it has no trace to name", path, k, v)
						}
					}
				}

				// the catalog's access line for this very call
				line := nextQuotePrices(t, pair.cat, before)
				tid, has := line["trace_id"]
				switch {
				case !tc.wantCatTrace:
					if has {
						t.Fatalf("%s: catalog-rpc (no SDK) logged trace_id %v", path, tid)
					}
				default:
					got, err := trace.TraceIDFromHex(fmt.Sprint(tid))
					if err != nil || !got.IsValid() {
						t.Fatalf("%s: catalog trace_id = %v, want a valid trace id (%v)", path, tid, err)
					}
					if got == ext {
						t.Fatalf("%s: the external caller's trace %s reached catalog-rpc through rpc", path, ext)
					}
					if tc.wantSameTrace && got != traceOf(t, h) {
						t.Fatalf("%s: catalog trace_id = %s, want rpc's %s", path, got, traceOf(t, h))
					}
				}
			}
			if strings.Contains(pair.cat.logs.String(), externalTrace) {
				t.Fatalf("the external trace id appears in catalog-rpc's log:\n%s", pair.cat.logs.String())
			}
		})
	}
}

// traceOf is the X-Trace-Id of a response or of an error's metadata; it must be a valid, non-zero trace id.
func traceOf(t *testing.T, h http.Header) trace.TraceID {
	t.Helper()
	id, err := trace.TraceIDFromHex(h.Get(server.TraceIDHeader))
	if err != nil || !id.IsValid() {
		t.Fatalf("X-Trace-Id = %q, want a valid trace id (%v)", h.Get(server.TraceIDHeader), err)
	}

	return id
}
