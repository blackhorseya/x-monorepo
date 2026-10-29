package oidc_test

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/oidc"
)

// Review findings on PR #277 (head d13b8e1), each pinned by a runtime assertion.

// P1: trust in the keys is renewed only once jwkset has accepted a refresh. An
// HTTP 200 whose content jwkset refuses (here a 900 KB RSA modulus that is not
// base64) must never make 25-hour-old keys look fresh, not even for the instant
// jwkset spends parsing it while other requests are verifying.
func TestRegression_RefusedRefreshNeverRenewsStaleKeys(t *testing.T) {
	clk := newClock()
	s := rsSigner(t, "k1")
	idp := newIdP(t, s)
	v := newVerifier(t, clk, idp, oidc.WithUnknownKIDInterval(5*time.Millisecond)) // 1h refresh; a short unknown-kid window repeats the race
	if _, err := verify(v, s.sign(t, goodClaims(clk, idp.url()))); err != nil {
		t.Fatal(err)
	}

	clk.Add(25 * time.Hour)
	tok := s.sign(t, goodClaims(clk, idp.url())) // really signed, and valid at the moved clock
	huge := strings.Repeat("A", 900_000) + "!"
	idp.rawKeys(s.jwk, map[string]any{"kty": "RSA", "kid": "k-broken", "n": huge, "e": "AQAB"})
	ghost := edSigner(t, "k-ghost").sign(t, goodClaims(clk, idp.url()))

	var done atomic.Bool
	var accepted atomic.Int64
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !done.Load() {
				if _, err := verify(v, tok); err == nil {
					accepted.Add(1)
				}
			}
		}()
	}
	for range 20 { // each unknown kid is a refetch that gets the refused content
		_, _ = verify(v, ghost)
		time.Sleep(10 * time.Millisecond)
	}
	done.Store(true)
	wg.Wait()

	if n := accepted.Load(); n != 0 {
		t.Fatalf("%d tokens were accepted with keys last refreshed 25h ago", n)
	}
}

// P2a: Close takes the verifier's gauge callback out of the meter provider.
func TestRegression_CloseUnregistersAgeGauge(t *testing.T) {
	clk := newClock()
	s := rsSigner(t, "k1")
	idp := newIdP(t, s)
	reader := sdkmetric.NewManualReader()
	v := newVerifier(t, clk, idp, oidcWithMeter(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	v.Warm(t.Context())
	if _, ok := collect(t, reader)["auth.jwks.age"]; !ok {
		t.Fatal("auth.jwks.age is not exported before Close")
	}

	v.Close()
	if m, ok := collect(t, reader)["auth.jwks.age"]; ok {
		if g, isGauge := m.Data.(metricdata.Gauge[float64]); !isGauge || len(g.DataPoints) != 0 {
			t.Fatalf("auth.jwks.age is still collected after Close: %+v", m.Data)
		}
	}
}

// P2b: after Close the keys already held still verify, but an unknown kid is
// rejected without a single HTTP request, so no new key can get in.
func TestRegression_CloseStopsFetchingAndAdmittingKeys(t *testing.T) {
	clk := newClock()
	known, added := rsSigner(t, "k-known"), esSigner(t, "k-added")
	idp := newIdP(t, known)
	v := newVerifier(t, clk, idp)
	v.Warm(t.Context())
	d0, j0 := idp.hits()

	v.Close()
	idp.setKeys(known, added)
	if _, err := verify(v, known.sign(t, goodClaims(clk, idp.url()))); err != nil {
		t.Fatalf("a key held before Close must keep verifying: %v", err)
	}
	if _, err := verify(v, added.sign(t, goodClaims(clk, idp.url()))); err == nil {
		t.Fatal("a key that appeared after Close was accepted")
	}
	if d, j := idp.hits(); d != d0 || j != j0 {
		t.Fatalf("requests after Close: discovery %d→%d, jwks %d→%d", d0, d, j0, j)
	}
}

// P2c: every JWKS attempt ends in exactly one counted result. A 200 whose
// content is refused is one error, not an ok followed by an error.
func TestRegression_RefusedContentCountsOnce(t *testing.T) {
	clk := newClock()
	s := rsSigner(t, "k1")
	idp := newIdP(t, s)
	reader := sdkmetric.NewManualReader()
	v := newVerifier(t, clk, idp, oidcWithMeter(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	if _, err := verify(v, s.sign(t, goodClaims(clk, idp.url()))); err != nil {
		t.Fatal(err)
	}

	idp.rawKeys(s.jwk, map[string]any{"kty": "RSA", "kid": "k-broken", "n": "!!", "e": "AQAB"})
	_, _ = verify(v, edSigner(t, "k-ghost").sign(t, goodClaims(clk, idp.url()))) // one refetch, refused

	_, hits := idp.hits()
	got := fetchCounts(t, collect(t, reader))
	if hits != 2 || got["ok"] != 1 || got["error"] != 1 {
		t.Fatalf("jwks requests = %d, counted ok=%d error=%d; want 2 requests, ok=1 error=1", hits, got["ok"], got["error"])
	}
}

func fetchCounts(t *testing.T, ms map[string]metricdata.Metrics) map[string]int64 {
	t.Helper()
	sum, ok := ms["auth.jwks.fetch"].Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("auth.jwks.fetch is %T", ms["auth.jwks.fetch"].Data)
	}
	out := map[string]int64{}
	for _, dp := range sum.DataPoints {
		res, _ := dp.Attributes.Value(attribute.Key("result"))
		out[res.AsString()] += dp.Value
	}

	return out
}

// P2b, in flight: Close aborts a JWKS request that is already running instead of
// leaving it to its 10 s timeout, and the lookup that was waiting on it fails.
func TestRegression_CloseAbortsInFlightFetch(t *testing.T) {
	clk := newClock()
	known, ghost := rsSigner(t, "k-known"), edSigner(t, "k-ghost")
	idp := newIdP(t, known)
	v := newVerifier(t, clk, idp)
	v.Warm(t.Context())

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	idp.set(func(p *fakeIdP) { p.blockJWKS = release })
	_, base := idp.hits()
	result := make(chan error, 1)
	go func() { _, err := verify(v, ghost.sign(t, goodClaims(clk, idp.url()))); result <- err }()
	waitFor(t, "the unknown-kid fetch to be in flight", func() bool { _, j := idp.hits(); return j > base })

	v.Close()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("the lookup of an unknown kid succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not abort the in-flight JWKS request")
	}
}
