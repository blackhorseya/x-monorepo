package oidc_test

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
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

// Review round 2 on PR #277 (head d17fedd).

// bodyEOFHook runs fn when the body has been read to its end: the response is
// fully downloaded but nothing has parsed it yet.
type bodyEOFHook struct {
	io.ReadCloser
	once sync.Once
	fn   func()
}

func (b *bodyEOFHook) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		b.once.Do(b.fn)
	}

	return n, err
}

type hookTransport struct {
	base http.RoundTripper
	hook func(*http.Response)
}

func (h hookTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := h.base.RoundTrip(r)
	if err == nil && h.hook != nil && strings.HasSuffix(r.URL.Path, "/keys") {
		h.hook(resp)
	}

	return resp, err
}

// Close is a linearization point: once it has returned, no refresh may commit a
// new keyset, including one whose response was already downloaded and was only
// waiting to be parsed.
func TestRegression_CloseBlocksCommitOfDownloadedKeys(t *testing.T) {
	clk := newClock()
	known, added := rsSigner(t, "k-known"), esSigner(t, "k-added")
	idp := newIdP(t, known)

	var v *oidc.Verifier
	var armed atomic.Bool
	client := *idp.srv.Client()
	client.Transport = hookTransport{base: http.DefaultTransport, hook: func(resp *http.Response) {
		if armed.Load() {
			resp.Body = &bodyEOFHook{ReadCloser: resp.Body, fn: func() { v.Close() }}
		}
	}}
	v = newVerifier(t, clk, idp, oidc.WithHTTPClient(&client))
	v.Warm(t.Context())

	idp.setKeys(known, added)
	armed.Store(true)
	_, err := verify(v, added.sign(t, goodClaims(clk, idp.url()))) // Close happens while its keys are downloaded
	if err == nil {
		t.Fatal("a key committed after Close was used")
	}
	if _, err := verify(v, added.sign(t, goodClaims(clk, idp.url()))); err == nil {
		t.Fatal("a key committed after Close is now a known key")
	}
	if _, err := verify(v, known.sign(t, goodClaims(clk, idp.url()))); err != nil {
		t.Fatalf("the key held before Close must still verify: %v", err)
	}
}

// Each failed attempt is one error whatever deadline the caller's http.Client
// puts on the request (a Client.Timeout replaces the request context), also
// with several issuers failing at once.
func TestRegression_FailedAttemptCountsOnceWithClientTimeout(t *testing.T) {
	clk := newClock()
	mk := func(name string) (*fakeIdP, *signer) {
		s := rsSigner(t, name)

		return newIdP(t, s), s
	}
	idpA, a := mk("ka")
	idpB, b := mk("kb")
	reader := sdkmetric.NewManualReader()
	cfg := config.OIDC{Enabled: true, Issuers: []config.OIDCIssuer{
		{Issuer: idpA.url(), Audience: clientID}, {Issuer: idpB.url(), Audience: clientID},
	}}
	v, err := oidc.New(cfg, oidc.WithClock(clk.Now), oidc.WithHTTPClient(&http.Client{Timeout: time.Second}),
		oidc.WithMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.Close)
	v.Warm(t.Context())

	for _, p := range []*fakeIdP{idpA, idpB} {
		p.set(func(p *fakeIdP) { p.failJWKS = true })
	}
	var wg sync.WaitGroup
	for _, c := range []struct {
		p *fakeIdP
		s *signer
	}{{idpA, a}, {idpB, b}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = verify(v, edSigner(t, "k-unknown").sign(t, goodClaims(clk, c.p.url()))) // one failing refetch
		}()
	}
	wg.Wait()

	got := fetchCounts(t, collect(t, reader))
	if got["ok"] != 2 || got["error"] != 2 {
		t.Fatalf("counted ok=%d error=%d; want ok=2 (the two warm fetches) error=2 (one failed refetch each)", got["ok"], got["error"])
	}
}

// Keys older than 24 h are never trusted, but that does not forbid recovering:
// an unknown kid still refetches within its window, and the verifier trusts
// again once a keyset has been accepted.
func TestRegression_StaleKeysRecoverThroughUnknownKid(t *testing.T) {
	clk := newClock()
	old, fresh := rsSigner(t, "k-old"), esSigner(t, "k-new")
	idp := newIdP(t, old)
	v := newVerifier(t, clk, idp) // production intervals
	if _, err := verify(v, old.sign(t, goodClaims(clk, idp.url()))); err != nil {
		t.Fatal(err)
	}

	clk.Add(25 * time.Hour)
	if _, err := verify(v, old.sign(t, goodClaims(clk, idp.url()))); err == nil {
		t.Fatal("a 25h-old key was trusted")
	}
	idp.setKeys(old, fresh) // the IdP is healthy again
	_, before := idp.hits()
	if _, err := verify(v, fresh.sign(t, goodClaims(clk, idp.url()))); err != nil {
		t.Fatalf("an unknown kid must recover the verifier: %v", err)
	}
	if _, after := idp.hits(); after != before+1 {
		t.Fatalf("recovery fetches = %d, want 1", after-before)
	}
	if _, err := verify(v, old.sign(t, goodClaims(clk, idp.url()))); err != nil {
		t.Fatalf("after an accepted refresh the keys are trusted again: %v", err)
	}
}
