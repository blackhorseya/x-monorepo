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

// startTracedPair runs a real catalog-rpc and an rpc, both with the SDK on (otel.endpoint set), and says so
// itself: a pair that quietly fell back to the no-op tracer would make every trace assertion below vacuous.
func startTracedPair(t *testing.T) (rpcURL string, cat *catalogProc) {
	t.Helper()
	endpoint := deadCollector(t)
	cat = startCatalogProc(t, "APP_OTEL_ENDPOINT="+endpoint, "APP_OTEL_INSECURE=true", "APP_OTEL_SERVICE_NAME=catalog-rpc")
	if !strings.Contains(cat.logs.String(), `"otel exporting"`) {
		t.Fatalf("catalog-rpc runs without the OTel SDK (no otel.endpoint taken):\n%s", cat.logs.String())
	}
	cfg := withCatalog(&config.AppConfig{OTel: config.OTel{Endpoint: endpoint, Insecure: true, ServiceName: "rpc"}}, cat)
	if cfg.OTel.Endpoint == "" {
		t.Fatal("rpc has no otel.endpoint: its tracer would be a no-op")
	}
	ts := rpcServer(t, cfg)
	seedProduct(t, ts.URL, "", "TR-1", 100, "USD")

	return ts.URL, cat
}

// quotePricesTraceIDs are the trace_ids of the catalog's access lines for QuotePrices, as of now.
func quotePricesTraceIDs(cat *catalogProc) []string {
	var ids []string
	for _, l := range strings.Split(cat.logs.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["msg"] == "rpc" && strings.HasSuffix(fmt.Sprint(m["procedure"]), "/QuotePrices") {
			ids = append(ids, fmt.Sprint(m["trace_id"]))
		}
	}

	return ids
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

// waitForQuotePrices waits (the child's stdout is copied by a goroutine) for a QuotePrices access line under id.
func waitForQuotePrices(t *testing.T, cat *catalogProc, id trace.TraceID) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, got := range quotePricesTraceIDs(cat) {
			if got == id.String() {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("catalog-rpc logged no QuotePrices line under trace %s; QuotePrices lines carry %v\n%s", id, quotePricesTraceIDs(cat), cat.logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// One trace spans rpc and catalog-rpc, and the trace is rpc's own: an external caller's traceparent is not
// continued at rpc (it only becomes a link), it is not forwarded to the catalog, and the catalog continues the
// trace rpc started. The X-Trace-Id a user reads off the response is the trace_id to search Tempo and Loki for.
func TestTrace_CatalogJoinsRPCsTraceAndRPCDoesNotTrustTheCaller(t *testing.T) {
	rpcURL, cat := startTracedPair(t)
	const external = "4bf92f3577b34da6a3ce929d0e0e4736"
	client := orderingv1connect.NewOrderServiceClient(http.DefaultClient, rpcURL)

	for name, items := range map[string][]*orderingv1.CreateOrderItem{
		"success": {{Sku: "TR-1", Quantity: 1}},
		"the catalog's business error (unknown sku)": {{Sku: "NOPE", Quantity: 1}},
	} {
		t.Run(name, func(t *testing.T) {
			req := connect.NewRequest(&orderingv1.CreateOrderRequest{CustomerId: "c-tr", Items: items})
			req.Header().Set("traceparent", "00-"+external+"-00f067aa0ba902b7-01")
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
			if (name == "success") != (err == nil) {
				t.Fatalf("CreateOrder error = %v", err)
			}

			got := traceOf(t, h)
			ext, _ := trace.TraceIDFromHex(external)
			if !ext.IsValid() {
				t.Fatal("the external trace id in the test is not valid")
			}
			if got == ext {
				t.Fatalf("rpc continued the external caller's trace %s; it must start its own", ext)
			}
			waitForQuotePrices(t, cat, got)
		})
	}
}
