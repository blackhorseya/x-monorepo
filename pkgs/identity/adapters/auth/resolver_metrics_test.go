package auth_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/adapters/auth"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
)

const (
	lookupsMetric = "identity.resolver.lookups"
	entriesMetric = "identity.resolver.cache.entries"
)

var allResults = []string{"hit", "miss", "db_ok", "db_miss", "error"}

func noopMeter() metric.MeterProvider { return noop.NewMeterProvider() }

func newResolver(t *testing.T, uc auth.ResolveAPIKeyUC, clk *clock, mp metric.MeterProvider) *auth.Resolver {
	t.Helper()
	r, err := auth.NewResolver(uc, clk, mp)
	if err != nil {
		t.Fatal(err)
	}

	return r
}

func collect(t *testing.T, reader sdkmetric.Reader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}

	return rm
}

// lookups returns the counter by result and fails if any data point carries
// anything but the single "result" attribute, or a value outside allResults.
func lookups(t *testing.T, rm metricdata.ResourceMetrics) map[string]int64 {
	t.Helper()
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != lookupsMetric {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok || !sum.IsMonotonic {
				t.Fatalf("%s data = %T monotonic=%v, want a monotonic Sum[int64]", m.Name, m.Data, ok && sum.IsMonotonic)
			}
			for _, dp := range sum.DataPoints {
				if dp.Attributes.Len() != 1 {
					t.Fatalf("%s attributes = %v, want exactly {result}", m.Name, dp.Attributes.ToSlice())
				}
				v, ok := dp.Attributes.Value("result")
				if !ok {
					t.Fatalf("%s attributes = %v, want key result", m.Name, dp.Attributes.ToSlice())
				}
				if !contains(allResults, v.AsString()) {
					t.Fatalf("result = %q, want one of %v", v.AsString(), allResults)
				}
				got[v.AsString()] += dp.Value
			}
		}
	}

	return got
}

// entries returns the cache-size gauge; it has no attributes at all.
func entries(t *testing.T, rm metricdata.ResourceMetrics) int64 {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != entriesMetric {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok || len(g.DataPoints) != 1 {
				t.Fatalf("%s data = %T, want one Gauge[int64] point", m.Name, m.Data)
			}
			if n := g.DataPoints[0].Attributes.Len(); n != 0 {
				t.Fatalf("%s has %d attributes, want none", m.Name, n)
			}

			return g.DataPoints[0].Value
		}
	}
	t.Fatalf("%s not exported", entriesMetric)

	return 0
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}

	return false
}

func assertLookups(t *testing.T, got, want map[string]int64) {
	t.Helper()
	for _, k := range allResults {
		if got[k] != want[k] {
			t.Errorf("lookups{result=%q} = %d, want %d (all: %v)", k, got[k], want[k], got)
		}
	}
}

// Each path through ResolveAPIKey bumps exactly the results the docs promise:
// hit | miss are the cache; every miss is followed by one of db_ok | db_miss | error.
func TestResolverMetrics_Results(t *testing.T) {
	c := context.Background()
	good, bad := "xk_"+strings.Repeat("g", 43), "xk_"+strings.Repeat("b", 43)
	uc := &counting{known: map[string]*dto.ResolveAPIKeyOutput{good: {AccountID: "acc-1", KeyID: "key-1"}}}
	clk := &clock{t: time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)}
	reader := sdkmetric.NewManualReader()
	r := newResolver(t, uc, clk, sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))

	_, _, _ = r.ResolveAPIKey(c, good) // miss + db_ok
	assertLookups(t, lookups(t, collect(t, reader)), map[string]int64{"miss": 1, "db_ok": 1})

	_, _, _ = r.ResolveAPIKey(c, good) // hit (a cached success)
	_, _, _ = r.ResolveAPIKey(c, bad)  // miss + db_miss
	_, _, _ = r.ResolveAPIKey(c, bad)  // hit (a cached refusal is still a hit)
	assertLookups(t, lookups(t, collect(t, reader)), map[string]int64{"hit": 2, "miss": 2, "db_ok": 1, "db_miss": 1})

	uc.fail = true
	other := "xk_" + strings.Repeat("o", 43)
	_, _, _ = r.ResolveAPIKey(c, other) // miss + error
	_, _, _ = r.ResolveAPIKey(c, other) // errors are not cached: miss + error again
	assertLookups(t, lookups(t, collect(t, reader)), map[string]int64{"hit": 2, "miss": 4, "db_ok": 1, "db_miss": 1, "error": 2})

	// An entry past its TTL is a miss again, not a hit.
	uc.fail = false
	clk.t = clk.t.Add(auth.DefaultCacheTTL)
	_, _, _ = r.ResolveAPIKey(c, good) // miss + db_ok
	assertLookups(t, lookups(t, collect(t, reader)), map[string]int64{"hit": 2, "miss": 5, "db_ok": 2, "db_miss": 1, "error": 2})
}

// With the cache disabled nothing can hit: every lookup is a miss.
func TestResolverMetrics_CacheDisabled(t *testing.T) {
	good := "xk_" + strings.Repeat("g", 43)
	uc := &counting{known: map[string]*dto.ResolveAPIKeyOutput{good: {AccountID: "acc-1", KeyID: "key-1"}}}
	reader := sdkmetric.NewManualReader()
	r, err := auth.NewResolverWithTTL(uc, &clock{t: time.Now()}, 0, sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		_, _, _ = r.ResolveAPIKey(context.Background(), good)
	}
	rm := collect(t, reader)
	assertLookups(t, lookups(t, rm), map[string]int64{"miss": 3, "db_ok": 3})
	if n := entries(t, rm); n != 0 {
		t.Fatalf("entries with the cache disabled = %d, want 0", n)
	}
}

// The cardinality guard: however many distinct keys come through, the counter
// has at most the five result series and the only attribute is "result" —
// never a key, prefix, hash, account or key id (lookups() fails on anything else).
func TestResolverMetrics_FixedCardinality(t *testing.T) {
	c := context.Background()
	known := map[string]*dto.ResolveAPIKeyOutput{}
	for i := range 50 {
		known[fmt.Sprintf("xk_%043d", i)] = &dto.ResolveAPIKeyOutput{AccountID: fmt.Sprintf("acc-%d", i), KeyID: fmt.Sprintf("key-%d", i)}
	}
	uc := &counting{known: known}
	reader := sdkmetric.NewManualReader()
	r := newResolver(t, uc, &clock{t: time.Now()}, sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	for i := range 100 { // 50 known + 50 unknown, twice each
		_, _, _ = r.ResolveAPIKey(c, fmt.Sprintf("xk_%043d", i))
		_, _, _ = r.ResolveAPIKey(c, fmt.Sprintf("xk_%043d", i))
	}
	uc.fail = true
	_, _, _ = r.ResolveAPIKey(c, "xk_"+strings.Repeat("z", 43))

	rm := collect(t, reader)
	got := lookups(t, rm)
	assertLookups(t, got, map[string]int64{"hit": 100, "miss": 101, "db_ok": 50, "db_miss": 50, "error": 1})
	series := 0
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == lookupsMetric {
				series += len(m.Data.(metricdata.Sum[int64]).DataPoints)
			}
		}
	}
	if series > len(allResults) {
		t.Fatalf("%s has %d series, want at most %d", lookupsMetric, series, len(allResults))
	}
}

// The gauge is the number of entries in the cache now: resolved and refused
// keys count, errors do not (they are not cached), and the MaxCacheEntries
// reset drops it back down.
func TestResolverMetrics_CacheEntries(t *testing.T) {
	c := context.Background()
	good, bad := "xk_"+strings.Repeat("g", 43), "xk_"+strings.Repeat("b", 43)
	uc := &counting{known: map[string]*dto.ResolveAPIKeyOutput{good: {AccountID: "acc-1", KeyID: "key-1"}}}
	reader := sdkmetric.NewManualReader()
	r := newResolver(t, uc, &clock{t: time.Now()}, sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))

	if n := entries(t, collect(t, reader)); n != 0 {
		t.Fatalf("entries on a fresh resolver = %d, want 0", n)
	}
	_, _, _ = r.ResolveAPIKey(c, good)
	_, _, _ = r.ResolveAPIKey(c, bad)
	_, _, _ = r.ResolveAPIKey(c, good) // a hit adds nothing
	if n := entries(t, collect(t, reader)); n != 2 {
		t.Fatalf("entries after a success and a refusal = %d, want 2", n)
	}
	uc.fail = true
	_, _, _ = r.ResolveAPIKey(c, "xk_"+strings.Repeat("o", 43))
	if n := entries(t, collect(t, reader)); n != 2 {
		t.Fatalf("entries after an infrastructure error = %d, want 2 (errors are not cached)", n)
	}
	uc.fail = false

	for i := range auth.MaxCacheEntries { // fills to the cap, then the next put resets
		_, _, _ = r.ResolveAPIKey(c, fmt.Sprintf("xk_%043d", i))
	}
	if n := entries(t, collect(t, reader)); n < 1 || n > auth.MaxCacheEntries {
		t.Fatalf("entries after overflowing the cap = %d, want 1..%d", n, auth.MaxCacheEntries)
	}
}
