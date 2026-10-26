// Package auth adapts the identity bounded context to the platform's auth
// interceptor: server.CredentialResolver over the ResolveAPIKey use case.
// It is the one place a BC is called from the platform, and only through the
// interface the platform declares (depguard keeps platform from importing us;
// apps/rpc wires the two together).
package auth

import (
	"context"
	"errors"
	"sync"
	"time"

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
// memory: a revoke, a disable or a role change takes effect within it. The
// hot path pays one lookup per key per TTL instead of one per request.
const DefaultCacheTTL = 30 * time.Second

// MaxCacheEntries bounds the cache; past it the whole cache is dropped (a
// flood of distinct unknown keys must not grow memory — rate limiting is the
// real defence, this is the backstop).
const MaxCacheEntries = 10_000

// Resolver answers the auth interceptor from the identity BC's keys, with a
// small TTL cache in front of the use case.
type Resolver struct {
	resolve ResolveAPIKeyUC
	clock   port.Clock
	ttl     time.Duration

	mu    sync.Mutex
	cache map[apikey.Hash]entry
}

type entry struct {
	principal contextx.Principal
	found     bool
	until     time.Time
}

// NewResolver builds the resolver with DefaultCacheTTL.
func NewResolver(resolve ResolveAPIKeyUC, clock port.Clock) *Resolver {
	return NewResolverWithTTL(resolve, clock, DefaultCacheTTL)
}

// NewResolverWithTTL is NewResolver with an explicit TTL (0 disables the cache).
func NewResolverWithTTL(resolve ResolveAPIKeyUC, clock port.Clock, ttl time.Duration) *Resolver {
	return &Resolver{resolve: resolve, clock: clock, ttl: ttl, cache: make(map[apikey.Hash]entry)}
}

// ResolveAPIKey implements server.CredentialResolver. A domain error from the
// use case (unknown, revoked, expired, account disabled, too short) is
// found=false — the caller never learns which; anything else is an
// infrastructure failure and is returned as err, uncached.
func (a *Resolver) ResolveAPIKey(c context.Context, key string) (contextx.Principal, bool, error) {
	h := apikey.HashSecret(key)
	now := a.clock.Now()
	if e, ok := a.get(h, now); ok {
		return e.principal, e.found, nil
	}

	out, err := a.resolve.Execute(c, dto.ResolveAPIKeyInput{Secret: key})
	var e entry
	switch {
	case err == nil:
		e = entry{found: true, principal: contextx.Principal{Subject: out.AccountID, Name: out.Name, Roles: out.Roles, CredentialID: out.KeyID}}
	case isDomainError(err):
		e = entry{found: false}
	default:
		return contextx.Principal{}, false, err
	}
	a.put(h, e, now)

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

func (a *Resolver) put(h apikey.Hash, e entry, now time.Time) {
	if a.ttl <= 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.cache) >= MaxCacheEntries {
		a.cache = make(map[apikey.Hash]entry)
	}
	e.until = now.Add(a.ttl)
	a.cache[h] = e
}

func isDomainError(err error) bool {
	var ke *kernel.Error
	return errors.As(err, &ke)
}
