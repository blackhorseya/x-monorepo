package oidc

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MicahParks/jwkset"
	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// JWKS rules (docs/identity-login.md §7). Key parsing, storage, the periodic
// refresh and the unknown-kid refetch are MicahParks/keyfunc and jwkset; this
// file is the thin adapter around them: discovery, the 24 h stale bound, the
// key-type check, the unknown-kid gate and the fetch metrics.
const (
	defaultRefreshInterval = time.Hour        // fixed refresh of every issuer's JWKS
	unknownKIDInterval     = 30 * time.Second // at most one unknown-kid refetch per issuer per window
	maxStale               = 24 * time.Hour   // keys keep verifying this long after the last successful fetch
	initWindow             = 30 * time.Second // at most one discovery attempt per issuer per window
	fetchTimeout           = 10 * time.Second // one discovery or JWKS request
	maxResponseLen         = 1 << 20          // bytes read from discovery / JWKS
	minRSABits             = 2048
)

var (
	errUnknownKey = errors.New("no signing key for this kid")
	errStale      = errors.New("signing keys are older than 24h without a successful refresh")
	errNotReady   = errors.New("issuer discovery has not succeeded yet")
	errClosed     = errors.New("verifier is closed")
)

type keyset struct {
	iss    config.OIDCIssuer
	client *http.Client // discovery
	jwks   *http.Client // handed to jwkset: counts every JWKS request
	now    func() time.Time
	fetchC metric.Int64Counter

	created         time.Time
	ctx             context.Context // ends the refresh goroutine (Verifier.Close)
	refreshInterval time.Duration
	unknownInterval time.Duration

	initMu   sync.Mutex
	kf       keyfunc.Keyfunc
	lim      *rate.Limiter
	lastInit time.Time

	mu          sync.Mutex
	lastSuccess time.Time // set only together with a keyset jwkset has accepted (acceptedStorage)

	failed sync.Map // request context → struct{}: attempts the transport already counted as errors

	group singleflight.Group
}

// ready returns the keyfunc, building it once discovery has succeeded. It does
// network I/O only on the first call that gets that far, and an IdP that is
// down just means the call fails (at most one attempt per initWindow) while
// every token is rejected.
func (ks *keyset) ready(c context.Context) (keyfunc.Keyfunc, error) {
	ks.initMu.Lock()
	defer ks.initMu.Unlock()
	if ks.kf != nil {
		return ks.kf, nil
	}
	if ks.ctx.Err() != nil {
		return nil, errClosed
	}
	now := ks.now()
	if !ks.lastInit.IsZero() && now.Sub(ks.lastInit) < initWindow {
		return nil, errNotReady
	}
	ks.lastInit = now

	c, cancel := ks.fetchContext(c)
	defer cancel()
	uri, err := ks.discover(c)
	if err != nil {
		ks.count(c, "error")
		contextx.FromContext(c).Warn("oidc: discovery failed", "issuer", ks.iss.Issuer, "error", err)

		return nil, err
	}

	ks.lim = rate.NewLimiter(rate.Every(ks.unknownInterval), 1)
	store, err := jwkset.NewStorageFromHTTP(uri, jwkset.HTTPClientStorageOptions{
		Client: ks.jwks,
		Ctx:    ks.ctx,
		// The first request must not fail construction: an IdP that is down at
		// startup is not fatal, the keys arrive with the next refresh.
		NoErrorReturnFirstHTTPReq: true,
		RefreshErrorHandler:       ks.onRefreshError,
		RefreshInterval:           ks.refreshInterval,
		HTTPTimeout:               fetchTimeout,
		Storage:                   acceptedStorage{Storage: jwkset.NewMemoryStorage(), ks: ks},
	})
	if err != nil {
		return nil, fmt.Errorf("jwks storage: %w", err)
	}
	client, err := jwkset.NewHTTPClient(jwkset.HTTPClientOptions{
		HTTPURLs:          map[string]jwkset.Storage{uri: store},
		RateLimitWaitMax:  fetchTimeout, // also bounds the refetch itself; the gate below keeps this from blocking requests
		RefreshUnknownKID: ks.lim,
	})
	if err != nil {
		return nil, fmt.Errorf("jwks client: %w", err)
	}
	kf, err := keyfunc.New(keyfunc.Options{
		Ctx:          ks.ctx,
		Storage:      client,
		UseWhitelist: []jwkset.USE{jwkset.UseSig, jwkset.USE("")}, // a JWK for encryption never verifies
	})
	if err != nil {
		return nil, fmt.Errorf("keyfunc: %w", err)
	}
	ks.kf = kf

	return kf, nil
}

// key returns the verification key for the token's kid and alg. An unknown kid
// refetches the JWKS only when this issuer's 30 s window allows it (checked
// here, so a throttled lookup is rejected at once instead of waiting inside
// the library), and concurrent lookups of the same kid share one call.
func (ks *keyset) key(c context.Context, tok *jwt.Token, kid, alg string) (any, error) {
	kf, err := ks.ready(c)
	if err != nil {
		return nil, err
	}
	// The trust check comes before the lookup: lastSuccess only moves forward,
	// when jwkset has replaced the keys, so a key read after this point comes
	// from the set that was trusted here or from a newer accepted one.
	if ks.age() > maxStale {
		return nil, errStale
	}
	v, err, _ := ks.group.Do(kid+"|"+alg, func() (any, error) {
		lc, cancel := ks.fetchContext(c)
		defer cancel()
		// After Close the keys already held still verify, but an unknown kid
		// never starts a request.
		if !hasKey(lc, kf, kid) && (ks.ctx.Err() != nil || ks.lim.Tokens() < 1) {
			return nil, errUnknownKey
		}

		return kf.KeyfuncCtx(lc)(tok)
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errUnknownKey, err)
	}
	if err := keyFitsAlg(v, alg); err != nil {
		return nil, err
	}

	return v, nil
}

func hasKey(c context.Context, kf keyfunc.Keyfunc, kid string) bool {
	all, err := kf.Storage().KeyReadAll(c)
	if err != nil {
		return false
	}
	for _, j := range all {
		if j.Marshal().KID == kid {
			return true
		}
	}

	return false
}

// keyFitsAlg is the kty/alg consistency check on the key actually returned:
// RS256 needs an RSA key of at least 2048 bits, ES256 a P-256 key, EdDSA an
// Ed25519 key. (keyfunc itself only compares a JWK's own "alg" with the
// token's.)
func keyFitsAlg(key any, alg string) error {
	switch k := key.(type) {
	case *rsa.PublicKey:
		if alg == algRS256 && k.N.BitLen() >= minRSABits {
			return nil
		}
	case *ecdsa.PublicKey:
		if alg == algES256 && k.Curve == elliptic.P256() {
			return nil
		}
	case ed25519.PublicKey:
		if alg == algEdDSA {
			return nil
		}
	}

	return fmt.Errorf("signing key does not fit alg %s", alg)
}

// Algorithms accepted, each tied to exactly one key type.
const (
	algRS256 = "RS256"
	algES256 = "ES256"
	algEdDSA = "EdDSA"
)

func algAllowed(alg string) bool {
	return alg == algRS256 || alg == algES256 || alg == algEdDSA
}

// age is the time since the last successful JWKS fetch, or since startup while
// there has been none: what the "no refresh for 2 h" alert watches, and what
// bounds stale-if-error.
func (ks *keyset) age() time.Duration {
	ks.mu.Lock()
	from := ks.lastSuccess
	ks.mu.Unlock()
	if from.IsZero() {
		from = ks.created
	}

	return ks.now().Sub(from)
}

func (ks *keyset) count(c context.Context, result string) {
	ks.fetchC.Add(c, 1, metric.WithAttributes(attribute.String("issuer", ks.iss.Issuer), attribute.String("result", result)))
}

// fetchContext bounds one discovery or JWKS request. It keeps the caller's
// values but not its cancellation (the result is shared by every request
// waiting on it), and it ends when the verifier is closed.
func (ks *keyset) fetchContext(c context.Context) (context.Context, context.CancelFunc) {
	fc, cancel := context.WithTimeout(context.WithoutCancel(c), fetchTimeout)
	stop := context.AfterFunc(ks.ctx, cancel)

	return fc, func() { stop(); cancel() }
}

// acceptedStorage is the one place a refresh counts as a success: jwkset calls
// KeyReplaceAll only after it has parsed every JWK of a response, so the
// last-success time moves in the same step as the keys it vouches for. A
// response jwkset refuses never gets here.
type acceptedStorage struct {
	jwkset.Storage
	ks *keyset
}

func (a acceptedStorage) KeyReplaceAll(c context.Context, keys []jwkset.JWK) error {
	a.ks.mu.Lock()
	defer a.ks.mu.Unlock()
	if err := a.Storage.KeyReplaceAll(c, keys); err != nil {
		return err
	}
	a.ks.lastSuccess = a.ks.now()
	a.ks.count(c, "ok")

	return nil
}

// onRefreshError is jwkset's handler for every failed refresh. The transport
// has already counted the failures it saw (no response, bad status, not a JWK
// Set); what is left is a well-formed response whose keys jwkset refused, which
// is counted here, so each attempt ends in exactly one result.
func (ks *keyset) onRefreshError(c context.Context, err error) {
	contextx.FromContext(c).Warn("oidc: jwks refresh failed", "issuer", ks.iss.Issuer, "error", err)
	if _, counted := ks.failed.LoadAndDelete(c); !counted {
		ks.count(c, "error")
	}
}

// countingTransport counts the JWKS requests that fail before jwkset sees any
// keys: no response, a status other than 200, or a body that is oversized or
// not a JWK Set. It does not decide that a fetch succeeded; only jwkset
// accepting the keys does (acceptedStorage).
type countingTransport struct {
	base http.RoundTripper
	ks   *keyset
}

func (t countingTransport) fail(req *http.Request) {
	t.ks.failed.Store(req.Context(), struct{}{}) // the handler gets this same context
	t.ks.count(req.Context(), "error")
}

func (t countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		t.fail(req)

		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		t.fail(req)

		return resp, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseLen+1))
	_ = resp.Body.Close()
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err != nil || len(body) > maxResponseLen || json.Unmarshal(body, &doc) != nil || doc.Keys == nil {
		t.fail(req)

		return nil, errors.New("jwks response is not a JWK Set")
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))

	return resp, nil
}

// discover reads the discovery document. Its issuer must be the configured one
// (otherwise a rogue document could point us at another issuer's keys) and its
// jwks_uri must pass the same http/https policy as the issuer.
func (ks *keyset) discover(c context.Context) (string, error) {
	req, err := http.NewRequestWithContext(c, http.MethodGet, strings.TrimRight(ks.iss.Issuer, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return "", err
	}
	resp, err := ks.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("discovery: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("discovery: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseLen+1))
	if err != nil || len(body) > maxResponseLen {
		return "", errors.New("discovery: unreadable or oversized response")
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
