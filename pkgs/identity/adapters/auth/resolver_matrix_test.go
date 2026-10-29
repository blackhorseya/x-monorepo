package auth_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/adapters/auth"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/apikey"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/infra/memory"
)

// The contract these tests hold the resolver to (docs/identity.md, "last_used_at"):
//
//   - Per replica, a key's claim is held for one interval, however full the claim
//     table is: no more than one write attempt per key per interval.
//   - The table holds MaxCacheEntries claims per generation; past that, new keys go
//     unstamped until the next generation. Live claims are never forgotten.
//   - Across replicas the database refuses a second stamp inside the interval, so the
//     column trails the real last use by less than two intervals — not one.
//   - A write that cannot start (all slots busy) is dropped and un-claimed, so a later
//     request stamps the key.
//
// Everything runs on a fixed clock and channels; nothing sleeps.

const interval = time.Minute

// seed adds n resolvable keys named prefix-i, with key ids prefix-i, to uc.
func seed(uc *counting, prefix string, n int) []string {
	secrets := make([]string, n)
	for i := range n {
		secrets[i] = fmt.Sprintf("xk_%s%040d", prefix, i)
		uc.known[secrets[i]] = &dto.ResolveAPIKeyOutput{AccountID: "acc-1", KeyID: fmt.Sprintf("%s-%d", prefix, i)}
	}

	return secrets
}

// TestResolver_LastUsed_CapacityMatrix: N-1, N and N+1 distinct keys in one window.
// Every key the table can hold is written once and keeps its claim; the one past the
// bound waits (is not written, and does not make the table forget anything).
func TestResolver_LastUsed_CapacityMatrix(t *testing.T) {
	c := context.Background()
	limit := auth.MaxCacheEntries

	for _, tc := range []struct {
		name     string
		distinct int
		written  int
	}{
		{"N-1", limit - 1, limit - 1},
		{"N", limit, limit},
		{"N+1", limit + 1, limit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLastUsedFixture(t, interval)
			secrets := seed(f.uc, "cap", tc.distinct)

			for _, secret := range secrets {
				_, _, _ = f.r.ResolveAPIKey(c, secret)
				f.r.Wait()
			}
			if got := f.touch.count(); got != tc.written {
				t.Fatalf("writes for %d distinct keys = %d, want %d", tc.distinct, got, tc.written)
			}

			// Same instant, every key again: nothing is due, including the key that was left out.
			for _, secret := range secrets {
				_, _, _ = f.r.ResolveAPIKey(c, secret)
				f.r.Wait()
			}
			if got := f.touch.count(); got != tc.written {
				t.Fatalf("writes after a second pass inside the window = %d, want %d (a claim was forgotten)", got, tc.written)
			}
		})
	}
}

// TestResolver_LastUsed_OverflowKeyRecoversNextGeneration: the key left out at N+1 is
// stamped once the window has rolled over.
func TestResolver_LastUsed_OverflowKeyRecoversNextGeneration(t *testing.T) {
	c := context.Background()
	f := newLastUsedFixture(t, interval)
	secrets := seed(f.uc, "ovf", auth.MaxCacheEntries+1)
	for _, secret := range secrets {
		_, _, _ = f.r.ResolveAPIKey(c, secret)
		f.r.Wait()
	}
	left := secrets[len(secrets)-1]
	if got := f.touch.count(); got != auth.MaxCacheEntries {
		t.Fatalf("writes = %d, want %d", got, auth.MaxCacheEntries)
	}

	f.clk.t = f.clk.t.Add(interval)
	_, _, _ = f.r.ResolveAPIKey(c, left)
	f.r.Wait()
	if got := f.touch.count(); got != auth.MaxCacheEntries+1 {
		t.Fatalf("the left-out key after the window: %d writes, want %d", got, auth.MaxCacheEntries+1)
	}
}

// TestResolver_LastUsed_ClaimBoundary: the claim lasts exactly one interval, across a
// generation rotation too. X is claimed at t0 (generation 1), A at t0+59s. At t0+60s
// the table rotates and A's claim moves to the old generation; it must still hold
// until A's own interval ends (t0+119s), not the rotation's.
func TestResolver_LastUsed_ClaimBoundary(t *testing.T) {
	c := context.Background()
	f := newLastUsedFixture(t, interval)
	a := seed(f.uc, "a", 1)[0]
	t0 := f.clk.t
	at := func(d time.Duration) { f.clk.t = t0.Add(d) }
	resolve := func(secret string) int {
		_, _, _ = f.r.ResolveAPIKey(c, secret)
		f.r.Wait()

		return f.touch.count()
	}

	at(0)
	if got := resolve(f.secret); got != 1 {
		t.Fatalf("X at t0: %d, want 1", got)
	}
	at(59 * time.Second)
	if got := resolve(a); got != 2 {
		t.Fatalf("A at t0+59s: %d, want 2", got)
	}
	at(interval - time.Nanosecond)
	if got := resolve(f.secret); got != 2 {
		t.Fatalf("X one nanosecond before its interval ends: %d, want 2", got)
	}
	at(interval) // the table rotates; X's claim is exactly one interval old
	if got := resolve(f.secret); got != 3 {
		t.Fatalf("X at exactly one interval: %d, want 3", got)
	}
	if got := resolve(a); got != 3 {
		t.Fatalf("A at t0+60s, after the rotation: %d, want 3 (its claim is 1s old)", got)
	}
	at(118 * time.Second)
	if got := resolve(a); got != 3 {
		t.Fatalf("A at t0+118s: %d, want 3 (still throttled)", got)
	}
	at(119 * time.Second)
	if got := resolve(a); got != 4 {
		t.Fatalf("A at t0+119s: %d, want 4 (its interval is over)", got)
	}
}

// replicas builds n resolvers over one memory repository holding one key, each with
// its own claim table and cache, the way n rpc pods share a database. Every write
// attempt is recorded with its time and whether the database took it.
type attempt struct {
	replica  int
	at       time.Time
	recorded bool
}

type recordingTouch struct {
	mu       sync.Mutex
	replica  int
	inner    auth.TouchAPIKeyUC
	clk      *clock
	attempts *[]attempt
}

func (r *recordingTouch) Execute(c context.Context, in dto.TouchAPIKeyInput) (*dto.TouchAPIKeyOutput, error) {
	out, err := r.inner.Execute(c, in)
	if err == nil {
		r.mu.Lock()
		*r.attempts = append(*r.attempts, attempt{replica: r.replica, at: r.clk.t, recorded: out.Recorded})
		r.mu.Unlock()
	}

	return out, err
}

type cluster struct {
	t        *testing.T
	clk      *clock
	keys     *memory.APIKeyRepository
	rs       []*auth.Resolver
	secret   string
	attempts []attempt
}

func newCluster(t *testing.T, n int) *cluster {
	t.Helper()
	cl := &cluster{t: t, clk: &clock{t: time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)}, keys: memory.NewAPIKeyRepository()}
	cl.secret = "xk_" + fmt.Sprintf("%043d", 7)
	k, err := apikey.New("key-1", "acc-1", "laptop", cl.secret, time.Time{}, cl.clk.t)
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.keys.Add(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	for i := range n {
		uc := &counting{known: map[string]*dto.ResolveAPIKeyOutput{cl.secret: {AccountID: "acc-1", KeyID: "key-1"}}}
		r := newResolver(t, uc, cl.clk, noopMeter())
		r.TrackLastUsed(&recordingTouch{replica: i, inner: usecase.NewTouchAPIKey(cl.keys, cl.clk, interval), clk: cl.clk, attempts: &cl.attempts}, interval, nil)
		cl.rs = append(cl.rs, r)
	}

	return cl
}

// use is one request against replica i at base+d.
func (cl *cluster) use(base time.Time, i int, d time.Duration) {
	cl.clk.t = base.Add(d)
	_, _, _ = cl.rs[i].ResolveAPIKey(context.Background(), cl.secret)
	cl.rs[i].Wait()
}

// shown is what ListApiKeys would show right now.
func (cl *cluster) shown() time.Time {
	k, err := cl.keys.FindByID(context.Background(), "key-1")
	if err != nil {
		cl.t.Fatal(err)
	}

	return k.LastUsedAt()
}

// TestResolver_LastUsed_TwoReplicaPhaseLag is the interleaving the review found:
// replica A stamps at t=0 and the key is then only used on replica B, whose window
// opens at t=59s. B's attempt is a database no-op (A stamped 59s ago) but B keeps its
// claim, so it tries again only at t=119s. The column therefore trails by 118s at
// t=118s — nearly two intervals, which is what the docs promise — and B made exactly
// two attempts for 61 requests, so the no-op does not turn into a write per request.
func TestResolver_LastUsed_TwoReplicaPhaseLag(t *testing.T) {
	cl := newCluster(t, 2)
	base := cl.clk.t

	cl.use(base, 0, 0)
	if got := cl.shown(); !got.Equal(base) {
		t.Fatalf("after A's stamp the column shows %v, want %v", got, base)
	}

	var worst time.Duration
	for s := 59; s <= 118; s++ {
		cl.use(base, 1, time.Duration(s)*time.Second)
		if lag := time.Duration(s)*time.Second - cl.shown().Sub(base); lag > worst {
			worst = lag
		}
	}
	if worst != 118*time.Second {
		t.Fatalf("worst lag before B retries = %v, want 118s (the documented phase effect)", worst)
	}
	if worst >= 2*interval {
		t.Fatalf("lag %v is not below two intervals", worst)
	}

	cl.use(base, 1, 119*time.Second)
	if got := cl.shown(); !got.Equal(base.Add(119 * time.Second)) {
		t.Fatalf("after B's retry the column shows %v, want t+119s", got.Sub(base))
	}

	want := []attempt{
		{replica: 0, at: base, recorded: true},
		{replica: 1, at: base.Add(59 * time.Second), recorded: false},
		{replica: 1, at: base.Add(119 * time.Second), recorded: true},
	}
	if len(cl.attempts) != len(want) {
		t.Fatalf("write attempts = %+v, want %d (one per replica per interval)", cl.attempts, len(want))
	}
	for i, w := range want {
		if cl.attempts[i] != w {
			t.Fatalf("attempt %d = %+v, want %+v", i, cl.attempts[i], w)
		}
	}
}

// TestResolver_LastUsed_TwoReplicaSweep: both replicas serve the key, interleaved at a
// step that is coprime with the interval, for twenty minutes. Whatever the phase, no
// replica attempts twice inside an interval, and the column never trails by two
// intervals or more.
func TestResolver_LastUsed_TwoReplicaSweep(t *testing.T) {
	for _, step := range []time.Duration{7 * time.Second, 13 * time.Second, 29 * time.Second} {
		t.Run(step.String(), func(t *testing.T) {
			cl := newCluster(t, 2)
			base := cl.clk.t
			var worst time.Duration
			stamped := false
			for n, d := 0, time.Duration(0); d <= 20*time.Minute; n, d = n+1, d+step {
				cl.use(base, n%2, d)
				if !stamped {
					stamped = !cl.shown().IsZero()
				}
				if lag := d - cl.shown().Sub(base); stamped && lag > worst {
					worst = lag
				}
			}
			if worst >= 2*interval {
				t.Fatalf("worst lag %v, want below two intervals (%v)", worst, 2*interval)
			}

			last := map[int]time.Time{}
			for _, a := range cl.attempts {
				if prev, ok := last[a.replica]; ok && a.at.Sub(prev) < interval {
					t.Fatalf("replica %d attempted at %v and again %v later, inside one interval", a.replica, prev.Sub(base), a.at.Sub(prev))
				}
				last[a.replica] = a.at
			}
			stamps := 0
			for _, a := range cl.attempts {
				if a.recorded {
					stamps++
				}
			}
			if limit := int(20*time.Minute/interval) * 2; stamps > limit+2 {
				t.Fatalf("%d stamps in 20 minutes, want at most %d (replicas x intervals)", stamps, limit+2)
			}
		})
	}
}

// TestResolver_LastUsed_SlotsFillAndRecover: with every write slot busy a further key's
// write is dropped and un-claimed — it neither starts nor blocks the lookup — and
// once the slots free, the same key is stamped by its next request.
func TestResolver_LastUsed_SlotsFillAndRecover(t *testing.T) {
	c := context.Background()
	f := newLastUsedFixture(t, interval)
	const slots = 8
	secrets := seed(f.uc, "slot", slots+1)
	f.touch.release = make(chan struct{})
	f.touch.started = make(chan string, slots+1)

	for _, secret := range secrets[:slots] {
		_, _, _ = f.r.ResolveAPIKey(c, secret)
	}
	for range slots {
		<-f.touch.started // every slot is now inside Execute, blocked
	}

	// The ninth key: the lookup returns and the write never starts.
	if _, found, err := f.r.ResolveAPIKey(c, secrets[slots]); err != nil || !found {
		t.Fatalf("lookup with every slot busy = %v, %v", found, err)
	}
	select {
	case id := <-f.touch.started:
		t.Fatalf("a ninth write started (%s) with every slot busy", id)
	default:
	}

	close(f.touch.release)
	f.r.Wait()
	if got := f.touch.count(); got != slots {
		t.Fatalf("writes once the slots drained = %d, want %d", got, slots)
	}

	// Same instant: the dropped key was un-claimed, so its next request writes.
	_, _, _ = f.r.ResolveAPIKey(c, secrets[slots])
	f.r.Wait()
	if got := f.touch.count(); got != slots+1 {
		t.Fatalf("the dropped key's next request: %d writes, want %d", got, slots+1)
	}
	// ...and now it holds its claim like any other.
	_, _, _ = f.r.ResolveAPIKey(c, secrets[slots])
	f.r.Wait()
	if got := f.touch.count(); got != slots+1 {
		t.Fatalf("after the recovery write: %d, want %d", got, slots+1)
	}
}
