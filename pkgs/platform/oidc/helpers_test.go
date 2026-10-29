package oidc_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.opentelemetry.io/otel/metric"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/oidc"
)

const clientID = "x-console"

// clock is the verifier's and the tokens' shared notion of now, so skew and
// cache boundaries are tested exactly instead of with sleeps.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// signer is a real key pair: tokens are really signed and really verified.
type signer struct {
	kid    string
	alg    string
	method jwt.SigningMethod
	priv   crypto.Signer
	jwk    map[string]any
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

var (
	keysOnce sync.Once
	rsaKey   *rsa.PrivateKey
	ecKey    *ecdsa.PrivateKey
	edPub    ed25519.PublicKey
	edKey    ed25519.PrivateKey
)

func initKeys(t *testing.T) {
	t.Helper()
	keysOnce.Do(func() {
		var err error
		if rsaKey, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
		if ecKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			panic(err)
		}
		if edPub, edKey, err = ed25519.GenerateKey(rand.Reader); err != nil {
			panic(err)
		}
	})
}

func rsSigner(t *testing.T, kid string) *signer {
	initKeys(t)

	return &signer{kid: kid, alg: "RS256", method: jwt.SigningMethodRS256, priv: rsaKey, jwk: map[string]any{
		"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
		"n": b64(rsaKey.N.Bytes()), "e": b64(big.NewInt(int64(rsaKey.E)).Bytes()),
	}}
}

func esSigner(t *testing.T, kid string) *signer {
	initKeys(t)
	pad := func(b []byte) []byte { return append(make([]byte, 32-len(b)), b...) }

	return &signer{kid: kid, alg: "ES256", method: jwt.SigningMethodES256, priv: ecKey, jwk: map[string]any{
		"kty": "EC", "kid": kid, "use": "sig", "alg": "ES256", "crv": "P-256",
		"x": b64(pad(ecKey.X.Bytes())), "y": b64(pad(ecKey.Y.Bytes())),
	}}
}

func edSigner(t *testing.T, kid string) *signer {
	initKeys(t)

	return &signer{kid: kid, alg: "EdDSA", method: jwt.SigningMethodEdDSA, priv: edKey, jwk: map[string]any{
		"kty": "OKP", "kid": kid, "use": "sig", "alg": "EdDSA", "crv": "Ed25519", "x": b64(edPub),
	}}
}

// sign makes a token with this signer's key; header overrides alg/kid when set.
func (s *signer) sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(s.method, claims)
	tok.Header["kid"] = s.kid
	out, err := tok.SignedString(s.priv)
	if err != nil {
		t.Fatal(err)
	}

	return out
}

// fakeIdP serves discovery and a JWKS, counts every hit and can be told to
// fail or to rotate keys.
type fakeIdP struct {
	srv *httptest.Server

	mu           sync.Mutex
	keys         []map[string]any
	discoveryHit int
	jwksHit      int
	failJWKS     bool
	cacheControl string
	discIssuer   string // overrides the issuer the discovery document claims
	jwksURI      string // overrides the jwks_uri the discovery document claims
}

func newIdP(t *testing.T, signers ...*signer) *fakeIdP {
	t.Helper()
	p := &fakeIdP{cacheControl: "max-age=3600"}
	p.setKeys(signers...)
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		p.discoveryHit++
		iss, uri := p.discIssuer, p.jwksURI
		p.mu.Unlock()
		if iss == "" {
			iss = p.srv.URL
		}
		if uri == "" {
			uri = p.srv.URL + "/keys"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": iss, "jwks_uri": uri})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.jwksHit++
		if p.failJWKS {
			http.Error(w, "boom", http.StatusServiceUnavailable)

			return
		}
		if p.cacheControl != "" {
			w.Header().Set("Cache-Control", p.cacheControl)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": p.keys})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)

	return p
}

func (p *fakeIdP) setKeys(signers ...*signer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys = nil
	for _, s := range signers {
		p.keys = append(p.keys, s.jwk)
	}
}

func (p *fakeIdP) rawKeys(keys ...map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys = keys
}

func (p *fakeIdP) set(f func(*fakeIdP)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f(p)
}

func (p *fakeIdP) hits() (discovery, jwks int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.discoveryHit, p.jwksHit
}

func (p *fakeIdP) url() string { return p.srv.URL }

func newVerifier(t *testing.T, clk *clock, p *fakeIdP, opts ...oidc.Option) *oidc.Verifier {
	t.Helper()
	cfg := config.OIDC{Enabled: true, Issuers: []config.OIDCIssuer{{Issuer: p.url(), Audience: clientID}}}
	v, err := oidc.New(cfg, append([]oidc.Option{oidc.WithClock(clk.Now), oidc.WithHTTPClient(p.srv.Client())}, opts...)...)
	if err != nil {
		t.Fatalf("oidc.New: %v", err)
	}

	return v
}

// goodClaims is a token that should verify; tests break one field at a time.
func goodClaims(clk *clock, iss string) jwt.MapClaims {
	now := clk.Now()

	return jwt.MapClaims{
		"iss": iss, "sub": "user-1", "aud": clientID,
		"iat": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(),
		"email": "a@example.com", "email_verified": true,
	}
}

func verify(v *oidc.Verifier, tok string) (oidc.Identity, error) {
	return v.Verify(context.Background(), tok)
}

func oidcWithMeter(mp metric.MeterProvider) oidc.Option { return oidc.WithMeterProvider(mp) }
