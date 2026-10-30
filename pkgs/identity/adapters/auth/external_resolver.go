package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/externalidentity"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// ResolveExternalUC and ProvisionExternalUC are the use cases behind ExternalResolver.
type (
	ResolveExternalUC   = usecasex.UseCase[dto.ResolveExternalInput, *dto.ResolveExternalOutput]
	ProvisionExternalUC = usecasex.UseCase[dto.ProvisionExternalInput, *dto.AccountOutput]
)

// ExternalIdentity is what the resolver needs of a verified OIDC token. It is
// a plain value, defined here at the consumer: whoever verifies the token
// (the platform's OIDC verifier, wired by the auth interceptor) fills it.
type ExternalIdentity struct {
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
	IssuedAt      time.Time // the token's iat; zero when the token had none
}

// ExternalPrincipal is the account a verified token speaks for.
type ExternalPrincipal struct {
	AccountID         string
	Name              string
	Roles             []string
	SessionsRevokedAt time.Time // zero: never revoked
}

// StaticMapping is an IssuerMapping from a fixed table; an issuer not in it is bound.
type StaticMapping map[string]externalidentity.Mapping

var _ port.IssuerMapping = StaticMapping(nil)

func (m StaticMapping) MappingFor(issuer string) externalidentity.Mapping {
	if v, ok := m[issuer]; ok {
		return v
	}

	return externalidentity.MappingBound
}

// External lookup results: the only values of the
// identity.external_resolver.lookups "result" attribute. Fixed, so the series
// count is too: nothing per issuer, subject or account becomes a label.
//
//	hit          served from the cache
//	miss         not in the cache; the lookup then ends in exactly one of the next four
//	db_ok        the account was found (cached for the TTL)
//	unbound      no binding, bound mode: refused, not cached
//	provisioned  no binding, auto_provision: the account was created
//	error        infrastructure failure: refused, not cached
const (
	extHit         = "hit"
	extMiss        = "miss"
	extDBOK        = "db_ok"
	extUnbound     = "unbound"
	extProvisioned = "provisioned"
	extError       = "error"
)

type externalEntry struct {
	principal ExternalPrincipal
	disabled  bool
	until     time.Time
}

// ExternalResolver maps a verified external identity to an account, with a
// small TTL cache in front of the use case. It is fail-closed: an unbound,
// disabled or revoked identity, a token without iat, and every infrastructure
// error are refusals; only a found account is cached, so an error or an
// "unbound" never outlives the request that saw it.
//
// A refusal is an error to be told apart with errors.Is:
// externalidentity.ErrNotFound (unbound: the caller may allow ClaimInvite),
// account.ErrDisabled, account.ErrSessionRevoked, account.ErrMissingIssuedAt;
// anything else is infrastructure.
type ExternalResolver struct {
	resolve   ResolveExternalUC
	provision ProvisionExternalUC
	mapping   port.IssuerMapping
	clock     port.Clock
	ttl       time.Duration
	lookups   metric.Int64Counter

	mu    sync.Mutex
	cache map[[2]string]externalEntry
}

// NewExternalResolver builds the resolver with DefaultCacheTTL (30 s): a
// disable, a role change or a revocation takes effect within it.
func NewExternalResolver(resolve ResolveExternalUC, provision ProvisionExternalUC, mapping port.IssuerMapping, clock port.Clock, mp metric.MeterProvider) (*ExternalResolver, error) {
	r := &ExternalResolver{resolve: resolve, provision: provision, mapping: mapping, clock: clock, ttl: DefaultCacheTTL, cache: make(map[[2]string]externalEntry)}

	meter := mp.Meter(scopeName)
	lookups, err := meter.Int64Counter("identity.external_resolver.lookups",
		metric.WithDescription("External identity lookups by result: hit | miss (cache), db_ok | unbound | provisioned | error (what a miss ended in)"),
		metric.WithUnit("{lookup}"))
	if err != nil {
		return nil, fmt.Errorf("create identity.external_resolver.lookups: %w", err)
	}
	r.lookups = lookups
	if _, err := meter.Int64ObservableGauge("identity.external_resolver.cache.entries",
		metric.WithDescription("Entries in this replica's external identity cache (bounded by MaxCacheEntries; expired ones linger until looked up again)"),
		metric.WithUnit("{entry}"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			o.Observe(int64(len(r.cache)))

			return nil
		})); err != nil {
		return nil, fmt.Errorf("create identity.external_resolver.cache.entries: %w", err)
	}

	return r, nil
}

func (r *ExternalResolver) record(c context.Context, result string) {
	r.lookups.Add(c, 1, metric.WithAttributes(attribute.String("result", result)))
}

// ResolveExternal returns the principal the verified token speaks for, or an
// error that refuses it (see ExternalResolver). The token's iat is checked on
// every call, cache hits included: the cache is keyed (issuer, subject) and
// two tokens of one user carry different iats.
func (r *ExternalResolver) ResolveExternal(c context.Context, id ExternalIdentity) (ExternalPrincipal, error) {
	if id.Issuer == "" {
		return ExternalPrincipal{}, externalidentity.ErrInvalidIssuer
	}
	if id.Subject == "" {
		return ExternalPrincipal{}, externalidentity.ErrInvalidSubject
	}
	key := [2]string{id.Issuer, id.Subject}
	now := r.clock.Now()

	e, ok := r.get(key, now)
	if ok {
		r.record(c, extHit)
	} else {
		r.record(c, extMiss)
		var err error
		if e, err = r.lookup(c, id, now); err != nil {
			return ExternalPrincipal{}, err
		}
		r.put(key, e)
	}
	if e.disabled {
		return ExternalPrincipal{}, account.ErrDisabled
	}
	if err := account.CheckSession(e.principal.SessionsRevokedAt, id.IssuedAt); err != nil {
		return ExternalPrincipal{}, err
	}
	e.principal.Roles = slices.Clone(e.principal.Roles)

	return e.principal, nil
}

// lookup asks the use case, provisioning on first sight when the issuer says
// so. Nothing it returns as an error is cached.
func (r *ExternalResolver) lookup(c context.Context, id ExternalIdentity, now time.Time) (externalEntry, error) {
	in := dto.ResolveExternalInput{Issuer: id.Issuer, Subject: id.Subject}
	out, err := r.resolve.Execute(c, in)
	if errors.Is(err, externalidentity.ErrNotFound) {
		if r.mapping.MappingFor(id.Issuer) != externalidentity.MappingAutoProvision {
			r.record(c, extUnbound)

			return externalEntry{}, err
		}
		// Losing the race to another replica (ErrDuplicate) is fine: the
		// identity exists now, so read it back either way.
		if _, perr := r.provision.Execute(c, dto.ProvisionExternalInput{Issuer: id.Issuer, Subject: id.Subject, Email: id.Email}); perr != nil && !errors.Is(perr, externalidentity.ErrDuplicate) {
			r.record(c, extError)

			return externalEntry{}, perr
		}
		if out, err = r.resolve.Execute(c, in); err == nil {
			r.record(c, extProvisioned)
		}
	}
	if err != nil {
		r.record(c, extError)

		return externalEntry{}, err
	}
	if out.AccountID == "" {
		r.record(c, extError)

		return externalEntry{}, fmt.Errorf("resolve external identity: empty account id")
	}
	r.record(c, extDBOK)

	return externalEntry{
		principal: ExternalPrincipal{AccountID: out.AccountID, Name: out.Name, Roles: slices.Clone(out.Roles), SessionsRevokedAt: out.SessionsRevokedAt},
		disabled:  out.Disabled,
		until:     now.Add(r.ttl),
	}, nil
}

func (r *ExternalResolver) get(key [2]string, now time.Time) (externalEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.cache[key]
	if !ok {
		return externalEntry{}, false
	}
	if !now.Before(e.until) {
		delete(r.cache, key)

		return externalEntry{}, false
	}

	return e, true
}

func (r *ExternalResolver) put(key [2]string, e externalEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cache) >= MaxCacheEntries {
		r.cache = make(map[[2]string]externalEntry)
	}
	r.cache[key] = e
}
