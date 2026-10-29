// Package auth adapts the identity bounded context to the platform's auth
// interceptor: server.CredentialResolver over the ResolveAPIKey use case.
// It is the one place a BC is called from the platform, and only through the
// interface the platform declares (depguard keeps platform from importing us;
// apps/rpc wires the two together).
package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/apikey"
	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

var _ server.CredentialResolver = (*Resolver)(nil)

// ResolveAPIKeyUC is the use case behind the resolver.
type ResolveAPIKeyUC = usecasex.UseCase[dto.ResolveAPIKeyInput, *dto.ResolveAPIKeyOutput]

// DefaultCacheTTL bounds how long a resolved (or refused) key is served from
// memory: a revoke, a disable or a role change takes effect within it, and a
// database outage keeps serving keys verified within it (bounded staleness,
// never a key that was not verified). A key's own expiry is exact: an entry
// never outlives expires_at. The hot path pays one lookup per key per TTL
// instead of one per request.
const DefaultCacheTTL = 30 * time.Second

// MaxCacheEntries bounds the cache; past it the whole cache is dropped (a
// flood of distinct unknown keys must not grow memory — rate limiting is the
// real defence, this is the backstop).
const MaxCacheEntries = 10_000

// Lookup results: the only values of the identity.resolver.lookups "result"
// attribute (Prometheus: identity_resolver_lookups_total{result}). The set is
// fixed so the series count is too — nothing per key, prefix, hash, account or
// key id ever becomes a label.
//
//	hit      served from the cache (a cached success or a cached refusal)
//	miss     not in the cache (absent, past its TTL, or the cache is off); the lookup
//	         then goes to the use case and ends in exactly one of the next three
//	db_ok    the use case resolved the key (cached for the TTL)
//	db_miss  the use case refused it with a domain error: unknown, revoked, expired,
//	         account disabled, too short (the refusal is cached too)
//	error    infrastructure failure (database down, timeout): returned, not cached
//
// So hit+miss is every lookup, miss = db_ok+db_miss+error, and the cache hit
// ratio is hit/(hit+miss).
const (
	resultHit    = "hit"
	resultMiss   = "miss"
	resultDBOK   = "db_ok"
	resultDBMiss = "db_miss"
	resultError  = "error"
)

// scopeName is the OTel meter scope, named after the package like the other adapters.
const scopeName = "github.com/blackhorseya/x-monorepo/pkgs/identity/adapters/auth"

// Resolver answers the auth interceptor from the identity BC's keys, with a
// small TTL cache in front of the use case.
type Resolver struct {
	resolve ResolveAPIKeyUC
	clock   port.Clock
	ttl     time.Duration
	lookups metric.Int64Counter

	mu    sync.Mutex
	cache map[apikey.Hash]entry
}

type entry struct {
	principal contextx.Principal
	found     bool
	until     time.Time // the earlier of now+ttl and the key's expires_at
}

// NewResolver builds the resolver with DefaultCacheTTL. mp is the process's
// MeterProvider (never the global one): the resolver records
// identity.resolver.lookups{result} and observes identity.resolver.cache.entries.
func NewResolver(resolve ResolveAPIKeyUC, clock port.Clock, mp metric.MeterProvider) (*Resolver, error) {
	return NewResolverWithTTL(resolve, clock, DefaultCacheTTL, mp)
}

// NewResolverWithTTL is NewResolver with an explicit TTL (0 disables the cache).
func NewResolverWithTTL(resolve ResolveAPIKeyUC, clock port.Clock, ttl time.Duration, mp metric.MeterProvider) (*Resolver, error) {
	a := &Resolver{resolve: resolve, clock: clock, ttl: ttl, cache: make(map[apikey.Hash]entry)}

	meter := mp.Meter(scopeName)
	lookups, err := meter.Int64Counter("identity.resolver.lookups",
		metric.WithDescription("API key lookups by result: hit | miss (cache), db_ok | db_miss | error (what a miss ended in)"),
		metric.WithUnit("{lookup}"))
	if err != nil {
		return nil, fmt.Errorf("create identity.resolver.lookups: %w", err)
	}
	a.lookups = lookups

	if _, err := meter.Int64ObservableGauge("identity.resolver.cache.entries",
		metric.WithDescription("Entries in this replica's key cache (bounded by MaxCacheEntries; expired ones linger until looked up again)"),
		metric.WithUnit("{entry}"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(a.size()))

			return nil
		})); err != nil {
		return nil, fmt.Errorf("create identity.resolver.cache.entries: %w", err)
	}

	return a, nil
}

func (a *Resolver) record(c context.Context, result string) {
	a.lookups.Add(c, 1, metric.WithAttributes(attribute.String("result", result)))
}

func (a *Resolver) size() int {
	a.mu.Lock()
	defer a.mu.Unlock()

	return len(a.cache)
}

// ResolveAPIKey implements server.CredentialResolver. A domain error from the
// use case (unknown, revoked, expired, account disabled, too short) is
// found=false — the caller never learns which; anything else is an
// infrastructure failure and is returned as err, uncached.
func (a *Resolver) ResolveAPIKey(c context.Context, key string) (contextx.Principal, bool, error) {
	h := apikey.HashSecret(key)
	now := a.clock.Now()
	if e, ok := a.get(h, now); ok {
		a.record(c, resultHit)

		return e.principal, e.found, nil
	}
	a.record(c, resultMiss)

	out, err := a.resolve.Execute(c, dto.ResolveAPIKeyInput{Secret: key})
	var e entry
	switch {
	case err == nil:
		e = entry{found: true, principal: contextx.Principal{Subject: out.AccountID, Name: out.Name, Roles: out.Roles, CredentialID: out.KeyID}}
		a.record(c, resultDBOK)
		e.until = now.Add(a.ttl)
		if !out.ExpiresAt.IsZero() && out.ExpiresAt.Before(e.until) {
			e.until = out.ExpiresAt
		}
	case isDomainError(err):
		a.record(c, resultDBMiss)
		e = entry{found: false, until: now.Add(a.ttl)}
	default:
		a.record(c, resultError)

		return contextx.Principal{}, false, err
	}
	a.put(h, e)

	return e.principal, e.found, nil
}

func (a *Resolver) get(h apikey.Hash, now time.Time) (entry, bool) {
	if a.ttl <= 0 {
		return entry{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.cache[h]
	if !ok {
		return entry{}, false
	}
	if !now.Before(e.until) {
		delete(a.cache, h)
		return entry{}, false
	}

	return e, true
}

func (a *Resolver) put(h apikey.Hash, e entry) {
	if a.ttl <= 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.cache) >= MaxCacheEntries {
		a.cache = make(map[apikey.Hash]entry)
	}
	a.cache[h] = e
}

func isDomainError(err error) bool {
	var ke *kernel.Error
	return errors.As(err, &ke)
}
