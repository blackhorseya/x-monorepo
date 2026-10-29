package oidc_test

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func collect(t *testing.T, r *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}

	return out
}

// auth_jwks_fetch_total{issuer,result} counts every fetch, ok or not;
// auth_jwks_age_seconds{issuer} is the time since the last successful one
// (since startup while there has been none), which is what the 2 h alert reads.
func TestMetrics_FetchCounterAndAge(t *testing.T) {
	clk := newClock()
	s := rsSigner(t, "k1")
	idp := newIdP(t, s)
	reader := sdkmetric.NewManualReader()
	v := newVerifier(t, clk, idp, oidcWithMeter(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))

	// Nothing fetched yet: age counts from startup.
	clk.Add(90 * time.Minute)
	if got := age(t, collect(t, reader), idp.url()); got != 5400 {
		t.Fatalf("age before any fetch = %v, want 5400 (since startup)", got)
	}

	if _, err := verify(v, s.sign(t, goodClaims(clk, idp.url()))); err != nil {
		t.Fatal(err)
	}
	if got := age(t, collect(t, reader), idp.url()); got != 0 {
		t.Fatalf("age right after a successful fetch = %v, want 0", got)
	}

	idp.set(func(p *fakeIdP) { p.failJWKS = true })
	clk.Add(time.Hour + time.Second) // TTL over: the refetch fails
	_, _ = verify(v, s.sign(t, goodClaims(clk, idp.url())))
	clk.Add(time.Hour) // 2h1s since the last success: the alert condition
	if got := age(t, collect(t, reader), idp.url()); got != 7201 {
		t.Fatalf("age after 2h1s without a success = %v, want 7201", got)
	}

	m := collect(t, reader)["auth.jwks.fetch"]
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("auth.jwks.fetch is %T, want an int64 counter", m.Data)
	}
	got := map[string]int64{}
	for _, dp := range sum.DataPoints {
		iss, _ := dp.Attributes.Value(attribute.Key("issuer"))
		res, _ := dp.Attributes.Value(attribute.Key("result"))
		if iss.AsString() != idp.url() {
			t.Fatalf("issuer label = %q", iss.AsString())
		}
		got[res.AsString()] = dp.Value
	}
	if got["ok"] != 1 || got["error"] != 1 || len(got) != 2 {
		t.Fatalf("fetch counts = %v, want ok=1 error=1", got)
	}
}

func age(t *testing.T, ms map[string]metricdata.Metrics, issuer string) float64 {
	t.Helper()
	m, ok := ms["auth.jwks.age"]
	if !ok {
		t.Fatal("auth.jwks.age is not exported")
	}
	if m.Unit != "s" {
		t.Fatalf("unit = %q, want s (Prometheus: auth_jwks_age_seconds)", m.Unit)
	}
	g, ok := m.Data.(metricdata.Gauge[float64])
	if !ok {
		t.Fatalf("auth.jwks.age is %T, want a float64 gauge", m.Data)
	}
	for _, dp := range g.DataPoints {
		if v, _ := dp.Attributes.Value(attribute.Key("issuer")); v.AsString() == issuer {
			return dp.Value
		}
	}
	t.Fatalf("no auth.jwks.age point for %q", issuer)

	return 0
}
