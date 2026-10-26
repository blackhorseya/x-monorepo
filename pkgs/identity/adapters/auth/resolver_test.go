package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/adapters/auth"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/apikey"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

type clock struct{ t time.Time }

func (c *clock) Now() time.Time { return c.t }

var errDB = errors.New("connection refused")

// counting is a ResolveAPIKey use case that records calls and answers from a table.
type counting struct {
	calls int
	known map[string]*dto.ResolveAPIKeyOutput
	fail  bool
}

func (u *counting) Execute(_ context.Context, in dto.ResolveAPIKeyInput) (*dto.ResolveAPIKeyOutput, error) {
	u.calls++
	if u.fail {
		return nil, errDB
	}
	if out, ok := u.known[in.Secret]; ok {
		return out, nil
	}

	return nil, apikey.ErrNotFound
}

func TestResolver(t *testing.T) {
	c := context.Background()
	good, bad := "xk_"+strings.Repeat("g", 43), "xk_"+strings.Repeat("b", 43)
	uc := &counting{known: map[string]*dto.ResolveAPIKeyOutput{good: {AccountID: "acc-1", Name: "ops", Roles: []string{"admin"}, KeyID: "key-1"}}}
	clk := &clock{t: time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)}
	var _ usecasex.UseCase[dto.ResolveAPIKeyInput, *dto.ResolveAPIKeyOutput] = uc
	r := auth.NewResolver(uc, clk)

	p, found, err := r.ResolveAPIKey(c, good)
	if err != nil || !found || p.Subject != "acc-1" || p.Name != "ops" || p.CredentialID != "key-1" || len(p.Roles) != 1 {
		t.Fatalf("resolve = %+v, %v, %v", p, found, err)
	}
	if p.Method != "" {
		t.Fatal("the interceptor sets Method, not the resolver")
	}
	// Within the TTL the use case is not asked again, for a hit nor a miss.
	if _, found, err := r.ResolveAPIKey(c, bad); err != nil || found {
		t.Fatalf("miss = %v, %v", found, err)
	}
	for range 5 {
		_, _, _ = r.ResolveAPIKey(c, good)
		_, _, _ = r.ResolveAPIKey(c, bad)
	}
	if uc.calls != 2 {
		t.Fatalf("calls within ttl = %d, want 2", uc.calls)
	}
	// Past the TTL the use case is asked again.
	clk.t = clk.t.Add(auth.DefaultCacheTTL)
	if _, found, _ := r.ResolveAPIKey(c, good); !found || uc.calls != 3 {
		t.Fatalf("after ttl: found %v calls %d", found, uc.calls)
	}
	// An infrastructure failure is an error, and is not cached.
	uc.fail = true
	if _, _, err := r.ResolveAPIKey(c, bad); !errors.Is(err, errDB) {
		t.Fatalf("db failure: %v", err)
	}
	if _, _, err := r.ResolveAPIKey(c, bad); !errors.Is(err, errDB) || uc.calls != 5 {
		t.Fatalf("db failure again: %v, calls %d", err, uc.calls)
	}
	uc.fail = false

	// TTL 0: no cache at all.
	uc2 := &counting{known: uc.known}
	r0 := auth.NewResolverWithTTL(uc2, clk, 0)
	for range 3 {
		if _, found, err := r0.ResolveAPIKey(c, good); err != nil || !found {
			t.Fatalf("no cache: %v, %v", found, err)
		}
	}
	if uc2.calls != 3 {
		t.Fatalf("no-cache calls = %d", uc2.calls)
	}

	// Bounded: past MaxCacheEntries distinct misses the cache is dropped, not grown.
	uc3 := &counting{known: uc.known}
	r3 := auth.NewResolver(uc3, clk)
	for i := range auth.MaxCacheEntries + 1 {
		_, _, _ = r3.ResolveAPIKey(c, "xk_"+strings.Repeat("x", 30)+strings.Repeat("0", 13-len(itoa(i)))+itoa(i))
	}
	_, _, _ = r3.ResolveAPIKey(c, good)
	before := uc3.calls
	_, _, _ = r3.ResolveAPIKey(c, good)
	if uc3.calls != before {
		t.Fatal("good key must be served from the fresh cache after the reset")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}

	return string(b)
}
