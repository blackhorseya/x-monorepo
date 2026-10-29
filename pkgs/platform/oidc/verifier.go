package oidc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

// leeway is the clock skew tolerated on exp and nbf.
const leeway = 60 * time.Second

var (
	// ErrUnknownIssuer means the token's iss is not one of the configured
	// issuers; no key lookup or network call was made for it.
	ErrUnknownIssuer = errors.New("oidc: token issuer is not configured")
	// ErrInvalidToken wraps every other rejection (signature, claims, algorithm,
	// unknown kid). The detail is for logs; clients see one rejection.
	ErrInvalidToken = errors.New("oidc: invalid token")
)

// Identity is a verified external identity — what the issuer vouches for, and
// nothing about what we make of it. IssuedAt is the token's iat: the identity
// BC compares it with an account's sessions_revoked_at, which is why a token
// without one is rejected.
type Identity struct {
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool // only ever true for a JSON boolean true
	IssuedAt      time.Time
}

// Verifier checks tokens against several issuers, each with its own audience
// and keys. It is safe for concurrent use.
type Verifier struct {
	issuers map[string]*keyset
	now     func() time.Time
	cancel  context.CancelFunc // ends the JWKS refresh goroutines
}

// Option customises New.
type Option func(*options)

type options struct {
	now    func() time.Time
	client *http.Client
	mp     metric.MeterProvider

	refreshInterval time.Duration
	unknownInterval time.Duration
}

// WithClock replaces time.Now (tests).
func WithClock(now func() time.Time) Option { return func(o *options) { o.now = now } }

// WithHTTPClient sets the client for discovery and JWKS requests (default: a
// 10 s timeout). Redirects are never followed, whatever the client.
func WithHTTPClient(c *http.Client) Option { return func(o *options) { o.client = c } }

// WithMeterProvider sets where auth.jwks.fetch and auth.jwks.age are recorded
// (default: the global provider).
func WithMeterProvider(mp metric.MeterProvider) Option { return func(o *options) { o.mp = mp } }

// New builds a verifier for cfg. It does no network I/O: an IdP that is down
// at startup is not a startup failure (the first requests are rejected and the
// keys are fetched once it answers; Warm tries early).
func New(cfg config.OIDC, opts ...Option) (*Verifier, error) {
	if !cfg.Enabled {
		return nil, errors.New("oidc: not enabled")
	}
	if len(cfg.Issuers) == 0 {
		return nil, errors.New("oidc: no issuers configured")
	}
	o := options{
		now: time.Now, client: &http.Client{Timeout: fetchTimeout}, mp: otel.GetMeterProvider(),
		refreshInterval: defaultRefreshInterval, unknownInterval: unknownKIDInterval,
	}
	for _, opt := range opts {
		opt(&o)
	}
	client := *o.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	meter := o.mp.Meter("github.com/blackhorseya/x-monorepo/pkgs/platform/oidc")
	fetches, err := meter.Int64Counter("auth.jwks.fetch",
		metric.WithDescription("JWKS fetch attempts per OIDC issuer, result ok / error."))
	if err != nil {
		return nil, fmt.Errorf("oidc: fetch counter: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	v := &Verifier{issuers: make(map[string]*keyset, len(cfg.Issuers)), now: o.now, cancel: cancel}
	for _, is := range cfg.Issuers {
		if err := config.CheckIssuerURL(is.Issuer, is.AllowInsecureIssuer); err != nil {
			cancel()

			return nil, fmt.Errorf("oidc: %w", err)
		}
		if is.Audience == "" {
			cancel()

			return nil, fmt.Errorf("oidc: issuer %s has no audience", is.Issuer)
		}
		if _, dup := v.issuers[is.Issuer]; dup {
			cancel()

			return nil, fmt.Errorf("oidc: duplicate issuer %s", is.Issuer)
		}
		ks := &keyset{
			iss: is, client: &client, now: o.now, fetchC: fetches, created: o.now(), ctx: ctx,
			refreshInterval: o.refreshInterval, unknownInterval: o.unknownInterval,
		}
		base := client.Transport
		if base == nil {
			base = http.DefaultTransport
		}
		jc := client
		jc.Transport = countingTransport{base: base, ks: ks}
		ks.jwks = &jc
		v.issuers[is.Issuer] = ks
	}

	if _, err := meter.Float64ObservableGauge("auth.jwks.age",
		metric.WithUnit("s"),
		metric.WithDescription("Seconds since the last successful JWKS fetch per OIDC issuer (since startup while there has been none)."),
		metric.WithFloat64Callback(func(_ context.Context, obs metric.Float64Observer) error {
			for iss, ks := range v.issuers {
				obs.Observe(ks.age().Seconds(), metric.WithAttributes(attribute.String("issuer", iss)))
			}

			return nil
		})); err != nil {
		cancel()

		return nil, fmt.Errorf("oidc: age gauge: %w", err)
	}

	return v, nil
}

// Warm discovers every issuer and fetches its keys now so the first request
// does not pay for it. Best-effort: a failure is logged and counted, nothing
// more, and the verifier retries on its own.
func (v *Verifier) Warm(c context.Context) {
	for _, ks := range v.issuers {
		_, _ = ks.ready(c)
	}
}

// Close stops the background JWKS refresh. The verifier keeps verifying with
// the keys it holds, but they no longer refresh.
func (v *Verifier) Close() { v.cancel() }

type claims struct {
	jwt.RegisteredClaims
	Email         any `json:"email,omitempty"`
	EmailVerified any `json:"email_verified,omitempty"`
}

// Verify checks token and returns the identity it carries. The issuer picks
// the keys and audience; everything else — algorithm, signature, iss, aud,
// exp, nbf (60 s skew), sub and iat present — is checked against that issuer's
// rules. It makes no network call for a token whose issuer or algorithm is
// not acceptable.
func (v *Verifier) Verify(c context.Context, token string) (Identity, error) {
	var unverified jwt.RegisteredClaims
	parsed, _, err := jwt.NewParser().ParseUnverified(token, &unverified)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	// iss is untrusted here: it only selects which issuer's rules apply, and the
	// token is then verified in full against them.
	ks, ok := v.issuers[unverified.Issuer]
	if !ok {
		return Identity{}, ErrUnknownIssuer
	}
	alg, _ := parsed.Header["alg"].(string)
	if !algAllowed(alg) {
		return Identity{}, fmt.Errorf("%w: algorithm %q is not accepted", ErrInvalidToken, alg)
	}
	kid, _ := parsed.Header["kid"].(string)
	if kid == "" {
		return Identity{}, fmt.Errorf("%w: no kid", ErrInvalidToken)
	}
	key, err := ks.key(c, parsed, kid, alg)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}

	var cl claims
	if _, err := jwt.ParseWithClaims(token, &cl, func(*jwt.Token) (any, error) { return key, nil },
		jwt.WithValidMethods([]string{alg}),
		jwt.WithIssuer(ks.iss.Issuer),
		jwt.WithAudience(ks.iss.Audience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(), // iat, when present, must not be in the future (the check below makes it required)
		jwt.WithLeeway(leeway),
		jwt.WithTimeFunc(v.now),
	); err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	if cl.Subject == "" {
		return Identity{}, fmt.Errorf("%w: no sub", ErrInvalidToken)
	}
	if cl.IssuedAt == nil {
		return Identity{}, fmt.Errorf("%w: no iat", ErrInvalidToken)
	}
	email, _ := cl.Email.(string)
	verified, _ := cl.EmailVerified.(bool)

	return Identity{
		Issuer:        ks.iss.Issuer,
		Subject:       cl.Subject,
		Email:         email,
		EmailVerified: verified,
		IssuedAt:      cl.IssuedAt.Time,
	}, nil
}
