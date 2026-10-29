package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// JWKS cache rules (docs/identity-login.md §7).
const (
	defaultTTL     = time.Hour
	minTTL         = 5 * time.Minute
	maxTTL         = 24 * time.Hour
	refetchWindow  = 30 * time.Second // at most one fetch attempt per issuer per window
	maxStale       = 24 * time.Hour   // last keys keep verifying this long after the last success
	fetchTimeout   = 10 * time.Second
	maxResponseLen = 1 << 20
)

var errUnknownKey = errors.New("no signing key for this kid")

// keyset is one issuer's discovery + JWKS state. Reads (every request) take mu
// briefly; fetches are serialised by fetchMu and run with mu released, so a
// slow IdP never blocks verification against keys already cached.
type keyset struct {
	iss    config.OIDCIssuer
	client *http.Client
	now    func() time.Time
	fetchC metric.Int64Counter

	created time.Time

	fetchMu sync.Mutex
	jwksURI string // discovered; cleared on any failure so the next attempt rediscovers

	mu          sync.RWMutex
	keys        map[string]verifyKey
	expiresAt   time.Time
	lastSuccess time.Time
	lastAttempt time.Time
}

type snapshot struct {
	keys        map[string]verifyKey
	expiresAt   time.Time
	lastSuccess time.Time
	lastAttempt time.Time
}

func (ks *keyset) snap() snapshot {
	ks.mu.RLock()
	defer ks.mu.RUnlock()

	return snapshot{ks.keys, ks.expiresAt, ks.lastSuccess, ks.lastAttempt}
}

// fresh answers from the cache alone: the kid is known and the cache has not
// expired.
func (s snapshot) fresh(kid string, now time.Time) (verifyKey, bool) {
	if now.Before(s.expiresAt) {
		if k, ok := s.keys[kid]; ok {
			return k, true
		}
	}

	return verifyKey{}, false
}

// usable also accepts expired keys, but only within maxStale of the last
// successful fetch (stale-if-error).
func (s snapshot) usable(kid string, now time.Time) (verifyKey, bool) {
	if s.lastSuccess.IsZero() || now.Sub(s.lastSuccess) > maxStale {
		return verifyKey{}, false
	}
	k, ok := s.keys[kid]

	return k, ok
}

// key returns the verification key for kid: from the cache when it is fresh,
// else after a refetch the 30 s window allows, else from stale keys. An
// unknown kid is never guessed.
func (ks *keyset) key(c context.Context, kid string) (verifyKey, error) {
	if k, ok := ks.snap().fresh(kid, ks.now()); ok {
		return k, nil
	}
	ks.refresh(c, kid)
	if k, ok := ks.snap().usable(kid, ks.now()); ok {
		return k, nil
	}

	return verifyKey{}, errUnknownKey
}

// refresh fetches when the cache cannot answer for kid and the window allows.
// Concurrent callers queue on fetchMu and re-check, so a rotation seen by many
// requests at once costs one fetch.
func (ks *keyset) refresh(c context.Context, kid string) {
	ks.fetchMu.Lock()
	defer ks.fetchMu.Unlock()

	now := ks.now()
	s := ks.snap()
	if _, ok := s.fresh(kid, now); ok {
		return // somebody fetched while we waited
	}
	if !s.lastAttempt.IsZero() && now.Sub(s.lastAttempt) < refetchWindow {
		return
	}

	ks.mu.Lock()
	ks.lastAttempt = now
	ks.mu.Unlock()

	keys, ttl, err := ks.fetch(c)
	if err != nil {
		ks.jwksURI = ""
		ks.fetchC.Add(c, 1, metric.WithAttributes(attribute.String("issuer", ks.iss.Issuer), attribute.String("result", "error")))
		contextx.FromContext(c).Warn("oidc: jwks fetch failed", "issuer", ks.iss.Issuer, "error", err)

		return
	}
	ks.fetchC.Add(c, 1, metric.WithAttributes(attribute.String("issuer", ks.iss.Issuer), attribute.String("result", "ok")))
	done := ks.now()
	ks.mu.Lock()
	ks.keys, ks.expiresAt, ks.lastSuccess = keys, done.Add(ttl), done
	ks.mu.Unlock()
}

// age is the time since the last successful fetch, or since startup while
// there has been none: the number the "no refresh for 2 h" alert watches.
func (ks *keyset) age() time.Duration {
	s := ks.snap()
	from := s.lastSuccess
	if from.IsZero() {
		from = ks.created
	}

	return ks.now().Sub(from)
}

// fetch runs discovery (once, then remembered) and the JWKS request. It is
// detached from the caller's cancellation: the result is shared by every
// request waiting on it, so one client hanging up must not fail it.
func (ks *keyset) fetch(c context.Context) (map[string]verifyKey, time.Duration, error) {
	c, cancel := context.WithTimeout(context.WithoutCancel(c), fetchTimeout)
	defer cancel()

	if ks.jwksURI == "" {
		uri, err := ks.discover(c)
		if err != nil {
			return nil, 0, err
		}
		ks.jwksURI = uri
	}
	body, hdr, err := ks.get(c, ks.jwksURI)
	if err != nil {
		return nil, 0, fmt.Errorf("jwks: %w", err)
	}
	keys, err := parseJWKS(body)
	if err != nil {
		return nil, 0, err
	}

	return keys, cacheTTL(hdr.Get("Cache-Control")), nil
}

// discover reads the discovery document. Its issuer must be the configured one
// (otherwise another issuer's keys could be planted by a rogue document) and
// its jwks_uri must pass the same http/https policy as the issuer.
func (ks *keyset) discover(c context.Context) (string, error) {
	body, _, err := ks.get(c, strings.TrimRight(ks.iss.Issuer, "/")+"/.well-known/openid-configuration")
	if err != nil {
		return "", fmt.Errorf("discovery: %w", err)
	}
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("discovery: not JSON: %w", err)
	}
	if doc.Issuer != ks.iss.Issuer {
		return "", errors.New("discovery: document names a different issuer")
	}
	if err := config.CheckEndpointURL(doc.JWKSURI, ks.iss.AllowInsecureIssuer); err != nil {
		return "", fmt.Errorf("discovery: jwks_uri: %w", err)
	}

	return doc.JWKSURI, nil
}

func (ks *keyset) get(c context.Context, url string) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(c, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := ks.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseLen+1))
	if err != nil {
		return nil, nil, err
	}
	if len(body) > maxResponseLen {
		return nil, nil, errors.New("response too large")
	}

	return body, resp.Header, nil
}

// cacheTTL is the Cache-Control max-age clamped to [5m, 24h]; without a usable
// max-age it is 1 h.
func cacheTTL(cacheControl string) time.Duration {
	for _, d := range strings.Split(cacheControl, ",") {
		name, val, ok := strings.Cut(strings.TrimSpace(d), "=")
		if !ok || !strings.EqualFold(name, "max-age") {
			continue
		}
		secs, err := strconv.ParseInt(strings.Trim(val, `"`), 10, 64)
		if err != nil || secs < 0 {
			break
		}
		ttl := time.Duration(min(secs, int64(maxTTL/time.Second))) * time.Second

		return min(max(ttl, minTTL), maxTTL)
	}

	return defaultTTL
}
