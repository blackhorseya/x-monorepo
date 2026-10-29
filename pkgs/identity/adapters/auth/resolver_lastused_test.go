package auth_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/adapters/auth"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
)

// touching is a TouchAPIKey use case that records the key ids it was asked to touch.
type touching struct {
	mu      sync.Mutex
	ids     []string
	err     error
	release chan struct{} // non-nil: Execute blocks until it is closed
}

func (u *touching) Execute(_ context.Context, in dto.TouchAPIKeyInput) (*dto.TouchAPIKeyOutput, error) {
	if u.release != nil {
		<-u.release
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.ids = append(u.ids, in.KeyID)
	if u.err != nil {
		return nil, u.err
	}

	return &dto.TouchAPIKeyOutput{Recorded: true}, nil
}

func (u *touching) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()

	return len(u.ids)
}

type lastUsedFixture struct {
	uc     *counting
	r      *auth.Resolver
	clk    *clock
	touch  *touching
	logs   *bytes.Buffer
	secret string
}

func newLastUsedFixture(t *testing.T, interval time.Duration) *lastUsedFixture {
	t.Helper()
	secret := "xk_" + strings.Repeat("g", 43)
	uc := &counting{known: map[string]*dto.ResolveAPIKeyOutput{secret: {AccountID: "acc-1", KeyID: "key-1"}}}
	f := &lastUsedFixture{uc: uc, clk: &clock{t: time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)}, touch: &touching{}, logs: &bytes.Buffer{}, secret: secret}
	f.r = newResolver(t, uc, f.clk, noopMeter())
	f.r.TrackLastUsed(f.touch, interval, slog.New(slog.NewTextHandler(f.logs, nil)))

	return f
}

func TestResolver_LastUsed_ThrottledPerKey(t *testing.T) {
	c := context.Background()
	f := newLastUsedFixture(t, time.Minute)

	// A burst inside one window, cache hits included: one write.
	for range 50 {
		if _, found, err := f.r.ResolveAPIKey(c, f.secret); err != nil || !found {
			t.Fatalf("resolve = %v, %v", found, err)
		}
	}
	f.r.Wait()
	if f.touch.count() != 1 || f.touch.ids[0] != "key-1" {
		t.Fatalf("touches in the first window = %v, want one for key-1", f.touch.ids)
	}

	// Still inside the window after the cache entry expired: the throttle is
	// the resolver's own clock, not the cache's TTL.
	f.clk.t = f.clk.t.Add(auth.DefaultCacheTTL + time.Second)
	_, _, _ = f.r.ResolveAPIKey(c, f.secret)
	f.r.Wait()
	if f.touch.count() != 1 {
		t.Fatalf("touches after cache expiry inside the window = %d, want 1", f.touch.count())
	}

	// Past the window: a second write, then quiet again.
	f.clk.t = f.clk.t.Add(time.Minute)
	for range 5 {
		_, _, _ = f.r.ResolveAPIKey(c, f.secret)
	}
	f.r.Wait()
	if f.touch.count() != 2 {
		t.Fatalf("touches after the window = %d, want 2", f.touch.count())
	}
}

func TestResolver_LastUsed_OnlyForResolvedKeys(t *testing.T) {
	c := context.Background()
	f := newLastUsedFixture(t, time.Minute)

	_, _, _ = f.r.ResolveAPIKey(c, "xk_"+strings.Repeat("u", 43)) // unknown key
	f.r.Wait()
	if f.touch.count() != 0 {
		t.Fatalf("a refused key was touched: %v", f.touch.ids)
	}
}

func TestResolver_LastUsed_Disabled(t *testing.T) {
	c := context.Background()
	f := newLastUsedFixture(t, 0)

	_, _, _ = f.r.ResolveAPIKey(c, f.secret)
	f.r.Wait()
	if f.touch.count() != 0 {
		t.Fatalf("interval 0 must not write, got %v", f.touch.ids)
	}
}

func TestResolver_LastUsed_FailureNeverFailsTheLookup(t *testing.T) {
	c := context.Background()
	f := newLastUsedFixture(t, time.Minute)
	f.touch.err = errors.New("pq: connection refused for xk_secret-in-error")

	p, found, err := f.r.ResolveAPIKey(c, f.secret)
	f.r.Wait()
	if err != nil || !found || p.Subject != "acc-1" {
		t.Fatalf("resolve with a failing touch = %+v, %v, %v", p, found, err)
	}
	out := f.logs.String()
	if !strings.Contains(out, "last_used") {
		t.Fatalf("a failed write must be logged, got %q", out)
	}
	// R4: neither the presented secret, its prefix nor the driver's error text reaches the log.
	for _, leak := range []string{f.secret, f.secret[:8] + strings.Repeat("g", 8), "connection refused", "secret-in-error"} {
		if strings.Contains(out, leak) {
			t.Fatalf("log leaks %q: %s", leak, out)
		}
	}
	// A failed write is retried on a later request, not parked for a whole window.
	_, _, _ = f.r.ResolveAPIKey(c, f.secret)
	f.r.Wait()
	if f.touch.count() != 2 {
		t.Fatalf("touches after a failure = %d, want a retry (2)", f.touch.count())
	}
}

func TestResolver_LastUsed_DoesNotBlockTheRequest(t *testing.T) {
	c := context.Background()
	f := newLastUsedFixture(t, time.Minute)
	f.touch.release = make(chan struct{})

	done := make(chan struct{})
	go func() {
		_, _, _ = f.r.ResolveAPIKey(c, f.secret)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ResolveAPIKey waited for the last_used_at write")
	}
	close(f.touch.release)
	f.r.Wait()
	if f.touch.count() != 1 {
		t.Fatalf("touches = %d, want 1", f.touch.count())
	}
}

// A full claim table must not forget claims that are still inside their window:
// forgetting them sends every live key back to the database on its next request
// (the conditional UPDATE keeps the column right, but not the round trips).
func TestResolver_LastUsed_CapacityKeepsLiveClaims(t *testing.T) {
	c := context.Background()
	f := newLastUsedFixture(t, time.Minute)

	_, _, _ = f.r.ResolveAPIKey(c, f.secret)
	f.r.Wait()
	for i := range auth.MaxCacheEntries {
		secret := fmt.Sprintf("xk_%040d", i)
		f.uc.known[secret] = &dto.ResolveAPIKeyOutput{AccountID: "acc-1", KeyID: fmt.Sprintf("other-%d", i)}
		_, _, _ = f.r.ResolveAPIKey(c, secret)
		f.r.Wait()
	}
	if got := f.touch.count(); got > auth.MaxCacheEntries {
		t.Fatalf("claims past the table's bound were written: %d touches, bound %d", got, auth.MaxCacheEntries)
	}
	before := f.touch.count()

	// Same instant, same key: still inside its window, so no write, however full the table is.
	_, _, _ = f.r.ResolveAPIKey(c, f.secret)
	f.r.Wait()
	if f.touch.count() != before {
		t.Fatalf("a full table forgot a live claim: %d touches, want %d", f.touch.count(), before)
	}

	// Once the window has passed, the key is stamped again: a full table is not a stuck one.
	f.clk.t = f.clk.t.Add(time.Minute)
	_, _, _ = f.r.ResolveAPIKey(c, f.secret)
	f.r.Wait()
	if f.touch.count() != before+1 {
		t.Fatalf("after the window: %d touches, want %d", f.touch.count(), before+1)
	}
}
