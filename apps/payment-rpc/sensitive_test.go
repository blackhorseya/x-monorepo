package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	collectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	paymentbc "github.com/blackhorseya/x-monorepo/pkgs/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// otlpSink is an in-process OTLP/gRPC collector: whatever the SDK exports (spans, logs, metrics) lands
// here as JSON, so a test can look at the real telemetry of a real server (a no-op tracer would make
// "no marker in any span" true by construction).
type otlpSink struct {
	collectortrace.UnimplementedTraceServiceServer

	mu   sync.Mutex
	data map[string]*strings.Builder
	// drop makes the receiver accept every export and keep nothing: the collector-side fault the
	// anchors below must notice.
	drop bool
}

func newOTLPSink() *otlpSink {
	return &otlpSink{data: map[string]*strings.Builder{"spans": {}, "logs": {}, "metrics": {}}}
}

func (s *otlpSink) add(kind string, m proto.Message) {
	b, _ := protojson.Marshal(m)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.drop {
		return
	}
	s.data[kind].Write(b)
	s.data[kind].WriteByte('\n')
}

func (s *otlpSink) text(kind string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.data[kind].String()
}

func (s *otlpSink) Export(_ context.Context, req *collectortrace.ExportTraceServiceRequest) (*collectortrace.ExportTraceServiceResponse, error) {
	s.add("spans", req)

	return &collectortrace.ExportTraceServiceResponse{}, nil
}

// the three services share the method name Export, so logs and metrics get their own receivers
type logsSink struct {
	collectorlogs.UnimplementedLogsServiceServer
	s *otlpSink
}

func (l logsSink) Export(_ context.Context, req *collectorlogs.ExportLogsServiceRequest) (*collectorlogs.ExportLogsServiceResponse, error) {
	l.s.add("logs", req)

	return &collectorlogs.ExportLogsServiceResponse{}, nil
}

type metricsSink struct {
	collectormetrics.UnimplementedMetricsServiceServer
	s *otlpSink
}

func (m metricsSink) Export(_ context.Context, req *collectormetrics.ExportMetricsServiceRequest) (*collectormetrics.ExportMetricsServiceResponse, error) {
	m.s.add("metrics", req)

	return &collectormetrics.ExportMetricsServiceResponse{}, nil
}

// syncBuf is a goroutine-safe buffer for the process's stdout.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.String()
}

// captureStdout swaps os.Stdout for a pipe until the test ends. The root logger binds os.Stdout when
// the server is built, so call this BEFORE Initialize.
func captureStdout(t *testing.T) *syncBuf {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	buf := &syncBuf{}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(buf, r)
		close(done)
	}()
	t.Cleanup(func() {
		os.Stdout = old
		_ = w.Close()
		<-done
	})

	return buf
}

// leakyProvider is a provider whose every failure carries a synthetic secret in its own words, the way a
// real gateway's error can reflect the credential it was given. Every call fails; calls counts them and
// said keeps the text it returned (the positive control: the marker really went into the fake's answer).
type leakyProvider struct {
	*mock.Provider
	marker string

	mu       sync.Mutex
	calls    int
	said     []string
	verified []string // what VerifyWebhook was given
}

func (p *leakyProvider) fail(op string) error {
	err := fmt.Errorf("gateway %s refused api key %s", op, p.marker)
	p.mu.Lock()
	p.calls++
	p.said = append(p.said, err.Error())
	p.mu.Unlock()

	return err
}

func (p *leakyProvider) CreateCheckout(context.Context, dto.ProviderCheckoutInput) (dto.ProviderCheckoutOutput, error) {
	return dto.ProviderCheckoutOutput{}, p.fail("create_checkout")
}

func (p *leakyProvider) GetPayment(context.Context, string) (dto.ProviderPaymentOutput, error) {
	return dto.ProviderPaymentOutput{}, p.fail("get_payment")
}

func (p *leakyProvider) CancelCheckout(context.Context, string) error {
	return p.fail("cancel_checkout")
}

// VerifyWebhook records what the server handed it (the positive control of the webhook scenarios: the
// marker really arrived in the request) and then judges it like the mock does.
func (p *leakyProvider) VerifyWebhook(body []byte, signature string) (dto.ProviderWebhookOutput, error) {
	p.mu.Lock()
	p.verified = append(p.verified, string(body)+"\n"+signature)
	p.mu.Unlock()

	return p.Provider.VerifyWebhook(body, signature)
}

func (p *leakyProvider) Refund(context.Context, dto.ProviderRefundInput) error {
	return p.fail("refund")
}

// leaks names the artifacts that contain marker.
func leaks(artifacts map[string]string, marker string) []string {
	var out []string
	for name, text := range artifacts {
		if strings.Contains(text, marker) {
			out = append(out, name)
		}
	}
	sort.Strings(out)

	return out
}

// The scanner itself must be able to say "leaked", or every "nothing leaked" below proves nothing.
func TestLeaks_FindsAMarkerInAnyArtifact(t *testing.T) {
	got := leaks(map[string]string{"a": "clean", "b": "has MARK in it", "c": "MARK"}, "MARK")
	if len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Fatalf("leaks = %v, want [b c]", got)
	}
	if got := leaks(map[string]string{"a": "", "b": "x"}, "MARK"); len(got) != 0 {
		t.Fatalf("leaks = %v on clean artifacts", got)
	}
}

// anchors say, per observed artifact, what THIS call must have left in it. "The marker is absent" means
// something only for an artifact that is non-empty and shows the call it was watching; an artifact with
// no anchor at all is not observed either, so adding one to the scenario without an anchor fails.
var startAnchors = map[string]string{
	"response status line": "503",
	"response headers":     "payment.provider_unavailable",
	"response body":        "unavailable",
	"process log":          "payment.start_payment",
	"exported spans":       "StartPayment",
	"exported logs":        "payment.start_payment",
	"exported metrics":     "payment.start_payment",
	"/api/metrics":         "payment.start_payment",
}

// unobserved names the artifacts that cannot show the marker absent: empty, without the call's anchor,
// or without an anchor defined.
func unobserved(artifacts, anchors map[string]string) []string {
	var out []string
	for name, text := range artifacts {
		if a, ok := anchors[name]; !ok || a == "" || !strings.Contains(text, a) {
			out = append(out, name)
		}
	}
	sort.Strings(out)

	return out
}

func TestUnobserved_FlagsEmptyMissingAndUnanchoredArtifacts(t *testing.T) {
	a := map[string]string{"empty": "", "other call": "something else", "ok": "has ANCHOR", "no anchor defined": "text"}
	got := unobserved(a, map[string]string{"empty": "ANCHOR", "other call": "ANCHOR", "ok": "ANCHOR"})
	if want := []string{"empty", "no anchor defined", "other call"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unobserved = %v, want %v", got, want)
	}
}

const leakMarker = "SYNTH-SECRET-9d41c7e2b0"

// exchange is what the caller of a scenario's request sees.
type exchange struct{ status, headers, body string }

// driveStart makes StartPayment fail through the real entry: the provider fake fails every call.
func driveStart(t *testing.T, base string, cfg config.Postgres, _ *leakyProvider) exchange {
	t.Helper()
	seed(t, cfg, "pay-1", "ord-1", owner, nil)
	req, _ := http.NewRequest(http.MethodPost, base+"/api/payment.v1.PaymentService/StartPayment",
		strings.NewReader(`{"order_id":"ord-1","customer_id":"`+owner+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(server.APIKeyHeader, serviceKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	var hdr strings.Builder
	for k, vs := range res.Header {
		hdr.WriteString(k + ": " + strings.Join(vs, ",") + "\n")
	}
	if res.StatusCode != http.StatusServiceUnavailable || res.Header.Get("X-Error-Code") != "payment.provider_unavailable" {
		t.Fatalf("response = %d %q / %q; want the provider-unavailable failure", res.StatusCode, body, res.Header.Get("X-Error-Code"))
	}

	return exchange{status: res.Status, headers: hdr.String(), body: string(body)}
}

// leakScenario makes StartPayment fail through the REAL entry (the real server, its interceptors,
// Postgres, an exporting OTel SDK) with leakMarker in every provider error, and returns everything an
// outsider could see afterwards. dropExports makes the collector discard what the SDK sends it.
func leakScenario(t *testing.T, dropExports bool, drive func(t *testing.T, base string, cfg config.Postgres, prov *leakyProvider) exchange) (map[string]string, *leakyProvider) {
	t.Helper()
	sink := newOTLPSink()
	sink.drop = dropExports
	// the three OTLP services each have an Export method: one receiver per service, all feeding the sink
	gs := grpc.NewServer()
	collectortrace.RegisterTraceServiceServer(gs, sink)
	collectorlogs.RegisterLogsServiceServer(gs, logsSink{s: sink})
	collectormetrics.RegisterMetricsServiceServer(gs, metricsSink{s: sink})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = gs.Serve(ln) }()
	t.Cleanup(gs.Stop)

	stdout := captureStdout(t)
	prov := &leakyProvider{Provider: mock.New(mock.Options{}), marker: leakMarker}
	cfg := freshDB(t)
	srv, cleanup, err := Initialize(&config.AppConfig{
		HTTP:     config.HTTP{Addr: ":0"},
		Log:      config.Log{Level: "debug", Format: "json"},
		Auth:     requiredAuth,
		Postgres: cfg,
		OTel:     config.OTel{Endpoint: ln.Addr().String(), Insecure: true, ServiceName: "payment-rpc"},
	}, paymentbc.Fixed(prov))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	ex := drive(t, ts.URL, cfg, prov)
	_, metricsPage := get(t, ts.URL+server.MetricsPath)

	ts.Close()
	cleanup() // flushes the exporters

	return map[string]string{
		"response status line": ex.status,
		"response headers":     ex.headers,
		"response body":        ex.body,
		"process log":          stdout.String(),
		"exported spans":       sink.text("spans"),
		"exported logs":        sink.text("logs"),
		"exported metrics":     sink.text("metrics"),
		"/api/metrics":         metricsPage,
	}, prov
}

// docs/payment.md §7 Webhook 6 / #347: a provider's error text is untrusted and may reflect a credential.
// The marker must be in none of: the response the caller gets (status, headers, body), the process log,
// the exported spans / logs / metrics, the /api/metrics page. The log carries only fixed categories and
// codes. Every artifact must also show THIS call (anchors), or "absent" proves nothing.
func TestPayment_ProviderErrorTextNeverLeaves(t *testing.T) {
	artifacts, prov := leakScenario(t, false, driveStart)

	// positive control: the marker reached the fake and came back as its error
	prov.mu.Lock()
	calls, said := prov.calls, strings.Join(prov.said, "\n")
	prov.mu.Unlock()
	if calls == 0 || !strings.Contains(said, leakMarker) {
		t.Fatalf("the provider fake was called %d times and said %q: the marker never went through it", calls, said)
	}
	if got := unobserved(artifacts, startAnchors); len(got) != 0 {
		t.Fatalf("artifacts that do not show the failed call, so cannot show the marker absent: %v", got)
	}

	if got := leaks(artifacts, leakMarker); len(got) != 0 {
		t.Fatalf("the provider's error text reached: %v", got)
	}
}

// Fault injection: a collector that discards what it receives leaves the exported artifacts empty, and
// the anchor check must go red on exactly those (the process, the response and /api/metrics are
// unaffected). Without this, "no marker in the exported telemetry" could pass on telemetry that was
// never exported.
func TestPayment_DroppedExportsAreNotMistakenForCleanOnes(t *testing.T) {
	artifacts, _ := leakScenario(t, true, driveStart)

	got := unobserved(artifacts, startAnchors)
	want := []string{"exported logs", "exported metrics", "exported spans"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unobserved = %v, want exactly %v", got, want)
	}
	for _, name := range want {
		if artifacts[name] != "" {
			t.Fatalf("%s is %d bytes although the collector drops everything", name, len(artifacts[name]))
		}
	}
}
