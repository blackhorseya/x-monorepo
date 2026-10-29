package oidc_test

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/oidc"
)

// The three accepted algorithms verify with really-signed tokens and come out
// as the verified external identity — and nothing more (no account, no roles).
func TestVerify_ValidTokens(t *testing.T) {
	clk := newClock()
	rs, es, ed := rsSigner(t, "k-rs"), esSigner(t, "k-es"), edSigner(t, "k-ed")
	idp := newIdP(t, rs, es, ed)
	v := newVerifier(t, clk, idp)

	for _, s := range []*signer{rs, es, ed} {
		t.Run(s.alg, func(t *testing.T) {
			id, err := verify(v, s.sign(t, goodClaims(clk, idp.url())))
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			want := oidc.Identity{
				Issuer: idp.url(), Subject: "user-1", Email: "a@example.com", EmailVerified: true,
				IssuedAt: time.Unix(clk.Now().Unix(), 0),
			}
			if id.Issuer != want.Issuer || id.Subject != want.Subject || id.Email != want.Email ||
				id.EmailVerified != want.EmailVerified || !id.IssuedAt.Equal(want.IssuedAt) {
				t.Fatalf("identity = %+v, want %+v", id, want)
			}
		})
	}
}

// email_verified must be a real boolean true: absent or the string "true" is false.
func TestVerify_EmailVerifiedIsStrict(t *testing.T) {
	clk := newClock()
	s := rsSigner(t, "k1")
	idp := newIdP(t, s)
	v := newVerifier(t, clk, idp)
	for name, val := range map[string]any{"string": "true", "false": false, "number": 1} {
		c := goodClaims(clk, idp.url())
		c["email_verified"] = val
		id, err := verify(v, s.sign(t, c))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if id.EmailVerified {
			t.Fatalf("email_verified=%v must not read as verified", val)
		}
	}
	c := goodClaims(clk, idp.url())
	delete(c, "email_verified")
	delete(c, "email")
	id, err := verify(v, s.sign(t, c))
	if err != nil || id.EmailVerified || id.Email != "" {
		t.Fatalf("no email claims: %+v %v", id, err)
	}
}

func TestVerify_ClaimChecks(t *testing.T) {
	clk := newClock()
	s := rsSigner(t, "k1")
	idp := newIdP(t, s)
	v := newVerifier(t, clk, idp)
	now := clk.Now()

	for name, tc := range map[string]struct {
		mut  func(jwt.MapClaims)
		want bool // accepted
	}{
		"baseline":               {func(jwt.MapClaims) {}, true},
		"aud list contains ours": {func(c jwt.MapClaims) { c["aud"] = []string{"other", clientID} }, true},
		"aud other":              {func(c jwt.MapClaims) { c["aud"] = "someone-else" }, false},
		"aud list without ours":  {func(c jwt.MapClaims) { c["aud"] = []string{"a", "b"} }, false},
		"aud missing":            {func(c jwt.MapClaims) { delete(c, "aud") }, false},
		"iss differs by slash":   {func(c jwt.MapClaims) { c["iss"] = idp.url() + "/" }, false},
		"iss missing":            {func(c jwt.MapClaims) { delete(c, "iss") }, false},
		"sub missing":            {func(c jwt.MapClaims) { delete(c, "sub") }, false},
		"sub empty":              {func(c jwt.MapClaims) { c["sub"] = "" }, false},
		// iat is required: sessions_revoked_at (#271) is compared against it.
		"iat missing": {func(c jwt.MapClaims) { delete(c, "iat") }, false},
		"exp missing": {func(c jwt.MapClaims) { delete(c, "exp") }, false},
		// 60 s of skew either way, and not a second more.
		"exp 59s ago":  {func(c jwt.MapClaims) { c["exp"] = now.Add(-59 * time.Second).Unix() }, true},
		"exp 61s ago":  {func(c jwt.MapClaims) { c["exp"] = now.Add(-61 * time.Second).Unix() }, false},
		"nbf 59s away": {func(c jwt.MapClaims) { c["nbf"] = now.Add(59 * time.Second).Unix() }, true},
		"nbf 61s away": {func(c jwt.MapClaims) { c["nbf"] = now.Add(61 * time.Second).Unix() }, false},
		"nbf 1h away":  {func(c jwt.MapClaims) { c["nbf"] = now.Add(time.Hour).Unix() }, false},
	} {
		t.Run(name, func(t *testing.T) {
			c := goodClaims(clk, idp.url())
			tc.mut(c)
			_, err := verify(v, s.sign(t, c))
			if tc.want && err != nil {
				t.Fatalf("want accepted, got %v", err)
			}
			if !tc.want && err == nil {
				t.Fatal("want rejected, got accepted")
			}
		})
	}
}

// A token from an issuer that is not configured never reaches a key lookup.
func TestVerify_UnknownIssuer(t *testing.T) {
	clk := newClock()
	s := rsSigner(t, "k1")
	idp := newIdP(t, s)
	v := newVerifier(t, clk, idp)
	other := newIdP(t, s)

	_, err := verify(v, s.sign(t, goodClaims(clk, other.url())))
	if !errors.Is(err, oidc.ErrUnknownIssuer) {
		t.Fatalf("err = %v, want ErrUnknownIssuer", err)
	}
	if d, j := other.hits(); d+j != 0 {
		t.Fatalf("an unconfigured issuer was contacted: discovery=%d jwks=%d", d, j)
	}
	if _, err := verify(v, "not-a-jwt"); err == nil {
		t.Fatal("garbage must be rejected")
	}
}

// Algorithm confusion and key/alg mismatches all fail closed.
func TestVerify_AlgorithmConfusion(t *testing.T) {
	clk := newClock()
	rs, es := rsSigner(t, "k-rs"), esSigner(t, "k-es")
	idp := newIdP(t, rs, es)
	v := newVerifier(t, clk, idp)
	claims := func() jwt.MapClaims { return goodClaims(clk, idp.url()) }

	der, err := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})

	hmac := func(secret []byte, kid string) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims())
		tok.Header["kid"] = kid
		out, err := tok.SignedString(secret)
		if err != nil {
			t.Fatal(err)
		}

		return out
	}
	none := func(kid string) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodNone, claims())
		tok.Header["kid"] = kid
		out, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
		if err != nil {
			t.Fatal(err)
		}

		return out
	}
	signWith := func(m jwt.SigningMethod, key any, kid string) string {
		tok := jwt.NewWithClaims(m, claims())
		tok.Header["kid"] = kid
		out, err := tok.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}

		return out
	}

	for name, tok := range map[string]string{
		"HS256 keyed with the RSA public key PEM": hmac(pemKey, "k-rs"),
		"HS256 keyed with the RSA modulus":        hmac(rsaKey.N.Bytes(), "k-rs"),
		"HS256 with an arbitrary secret":          hmac([]byte("0123456789abcdef0123456789abcdef"), "k-rs"),
		"alg none":                                none("k-rs"),
		"RS256 token, kid names the EC key":       signWith(jwt.SigningMethodRS256, rsaKey, "k-es"), // RS256 against a kty=EC JWK
		"RS512 is not on the allow-list":          signWith(jwt.SigningMethodRS512, rsaKey, "k-rs"),
		"ES384 is not on the allow-list":          signWith(jwt.SigningMethodES384, ecKey, "k-es"),
		"PS256 is not on the allow-list":          signWith(jwt.SigningMethodPS256, rsaKey, "k-rs"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := verify(v, tok); err == nil {
				t.Fatal("accepted")
			}
		})
	}

	// A JWK whose own alg disagrees with its kty, or with the token's alg.
	t.Run("JWK alg disagrees with the token alg", func(t *testing.T) {
		clk := newClock()
		bad := esSigner(t, "k-bad")
		bad.jwk["alg"] = "RS256" // an EC key that claims to be an RS256 key
		idp := newIdP(t, bad)
		v := newVerifier(t, clk, idp)
		if _, err := verify(v, bad.sign(t, goodClaims(clk, idp.url()))); err == nil {
			t.Fatal("ES256 token accepted against a JWK that says alg=RS256")
		}
	})
	t.Run("JWK use is not sig", func(t *testing.T) {
		clk := newClock()
		enc := rsSigner(t, "k-enc")
		enc.jwk["use"] = "enc"
		idp := newIdP(t, enc)
		v := newVerifier(t, clk, idp)
		if _, err := verify(v, enc.sign(t, goodClaims(clk, idp.url()))); err == nil {
			t.Fatal("an encryption key verified a signature")
		}
	})
	t.Run("token without kid", func(t *testing.T) {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims())
		out, _ := tok.SignedString(rsaKey)
		if _, err := verify(v, out); err == nil {
			t.Fatal("a token without kid was accepted")
		}
	})
	t.Run("unusable JWKs do not poison the set", func(t *testing.T) {
		clk := newClock()
		good := rsSigner(t, "k-good")
		idp := newIdP(t)
		idp.rawKeys(map[string]any{"kty": "oct", "kid": "k-oct", "k": "AAAA"}, map[string]any{"kty": "RSA", "kid": "k-broken", "n": "!!", "e": "AQAB"}, good.jwk)
		v := newVerifier(t, clk, idp)
		if _, err := verify(v, good.sign(t, goodClaims(clk, idp.url()))); err != nil {
			t.Fatalf("good key rejected beside junk: %v", err)
		}
	})
}

// JWKS cache lifetime follows Cache-Control, clamped to [5m, 24h], 1h without one.
func TestJWKS_CacheLifetime(t *testing.T) {
	for name, tc := range map[string]struct {
		header string
		ttl    time.Duration
	}{
		"max-age 10m":           {"max-age=600", 10 * time.Minute},
		"no header":             {"", time.Hour},
		"no max-age":            {"public", time.Hour},
		"no-store":              {"no-store", time.Hour},
		"garbage max-age":       {"max-age=soon", time.Hour},
		"below the floor":       {"max-age=10", 5 * time.Minute},
		"max-age 0":             {"max-age=0", 5 * time.Minute},
		"above the ceiling":     {"max-age=999999", 24 * time.Hour},
		"with other directives": {"public, max-age=7200, must-revalidate", 2 * time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			clk := newClock()
			s := rsSigner(t, "k1")
			idp := newIdP(t, s)
			idp.set(func(p *fakeIdP) { p.cacheControl = tc.header })
			v := newVerifier(t, clk, idp)
			try := func() {
				t.Helper()
				if _, err := verify(v, s.sign(t, goodClaims(clk, idp.url()))); err != nil {
					t.Fatal(err)
				}
			}
			try()
			if _, j := idp.hits(); j != 1 {
				t.Fatalf("first verify fetched %d times, want 1", j)
			}
			clk.Add(tc.ttl - time.Second)
			try()
			if _, j := idp.hits(); j != 1 {
				t.Fatalf("1s before the TTL it refetched (%d fetches)", j)
			}
			clk.Add(time.Second)
			try()
			if _, j := idp.hits(); j != 2 {
				t.Fatalf("at the TTL it fetched %d times, want 2", j)
			}
		})
	}
}

// An unknown kid refetches at once, but at most once per 30 s per issuer; a kid
// that is still unknown afterwards is rejected, never guessed.
func TestJWKS_UnknownKidRefetchAndThrottle(t *testing.T) {
	clk := newClock()
	old, fresh := rsSigner(t, "k-old"), esSigner(t, "k-new")
	idp := newIdP(t, old)
	v := newVerifier(t, clk, idp)
	if _, err := verify(v, old.sign(t, goodClaims(clk, idp.url()))); err != nil {
		t.Fatal(err)
	}

	idp.setKeys(old, fresh)   // the IdP rotates
	clk.Add(31 * time.Second) // past the 30 s window of the initial fetch
	if _, err := verify(v, fresh.sign(t, goodClaims(clk, idp.url()))); err != nil {
		t.Fatalf("a rotated-in kid must be picked up immediately: %v", err)
	}
	if _, j := idp.hits(); j != 2 {
		t.Fatalf("fetches = %d, want 2", j)
	}

	ghost := edSigner(t, "k-ghost")
	clk.Add(10 * time.Second) // 10 s after the last fetch: inside the window
	if _, err := verify(v, ghost.sign(t, goodClaims(clk, idp.url()))); err == nil {
		t.Fatal("unknown kid accepted")
	}
	if _, j := idp.hits(); j != 2 {
		t.Fatalf("unknown kid inside 30 s refetched (%d fetches)", j)
	}

	idp.setKeys(old, fresh, ghost)
	clk.Add(10 * time.Second) // 20 s: still throttled even though the key now exists upstream
	if _, err := verify(v, ghost.sign(t, goodClaims(clk, idp.url()))); err == nil {
		t.Fatal("throttle bypassed")
	}
	clk.Add(10 * time.Second) // 30 s
	if _, err := verify(v, ghost.sign(t, goodClaims(clk, idp.url()))); err != nil {
		t.Fatalf("after 30 s the refetch must happen: %v", err)
	}
	if _, j := idp.hits(); j != 3 {
		t.Fatalf("fetches = %d, want 3", j)
	}
}

// A flood of tokens with a kid nobody has costs one fetch per window, and
// concurrent first sightings of a rotated key share a single fetch.
func TestJWKS_ConcurrentUnknownKidsShareOneFetch(t *testing.T) {
	clk := newClock()
	old, fresh := rsSigner(t, "k-old"), esSigner(t, "k-new")
	idp := newIdP(t, old)
	v := newVerifier(t, clk, idp)
	if _, err := verify(v, old.sign(t, goodClaims(clk, idp.url()))); err != nil {
		t.Fatal(err)
	}
	idp.setKeys(old, fresh)
	clk.Add(time.Minute)

	tok := fresh.sign(t, goodClaims(clk, idp.url()))
	ghost := edSigner(t, "k-ghost").sign(t, goodClaims(clk, idp.url()))
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for range 50 {
		wg.Add(2)
		go func() { defer wg.Done(); _, err := verify(v, tok); errs <- err }()
		go func() { defer wg.Done(); _, err := verify(v, ghost); _ = err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("rotated kid rejected under concurrency: %v", err)
		}
	}
	if _, j := idp.hits(); j != 2 {
		t.Fatalf("fetches = %d, want 2 (initial + one shared refetch)", j)
	}
}

// When a refetch fails the old keys keep working for up to 24 h after the last
// success; an unknown kid is rejected throughout, and after 24 h so is
// everything.
func TestJWKS_StaleIfError(t *testing.T) {
	clk := newClock()
	s := rsSigner(t, "k1")
	idp := newIdP(t, s)
	v := newVerifier(t, clk, idp)
	tok := func() string { return s.sign(t, goodClaims(clk, idp.url())) }
	if _, err := verify(v, tok()); err != nil {
		t.Fatal(err)
	}

	idp.set(func(p *fakeIdP) { p.failJWKS = true })
	clk.Add(time.Hour + time.Second) // TTL over, refetch fails
	if _, err := verify(v, tok()); err != nil {
		t.Fatalf("stale key must keep working while the IdP is down: %v", err)
	}
	if _, j := idp.hits(); j != 2 {
		t.Fatalf("expected one failed refetch, fetches = %d", j)
	}
	other := esSigner(t, "k-unknown")
	clk.Add(time.Minute)
	if _, err := verify(v, other.sign(t, goodClaims(clk, idp.url()))); err == nil {
		t.Fatal("an unknown kid was accepted while serving stale keys")
	}

	clk.Add(22*time.Hour + 58*time.Minute) // 23h59m1s after the last success: inside 24 h
	if _, err := verify(v, tok()); err != nil {
		t.Fatalf("inside 24 h of the last success the stale key must still verify: %v", err)
	}
	clk.Add(2 * time.Minute) // 24h1m1s after the last success
	if _, err := verify(v, tok()); err == nil {
		t.Fatal("a key older than 24 h without a successful refetch was still trusted")
	}

	// Recovery: the IdP comes back and the next window works again.
	idp.set(func(p *fakeIdP) { p.failJWKS = false })
	clk.Add(30 * time.Second)
	if _, err := verify(v, tok()); err != nil {
		t.Fatalf("no recovery after the IdP returned: %v", err)
	}
}

// An IdP that is down at startup is not fatal: New succeeds, requests fail
// closed, and the first fetch after the throttle window recovers.
func TestJWKS_StartupFetchFailureIsNotFatal(t *testing.T) {
	clk := newClock()
	s := rsSigner(t, "k1")
	idp := newIdP(t, s)
	idp.set(func(p *fakeIdP) { p.failJWKS = true })
	v := newVerifier(t, clk, idp)
	v.Warm(t.Context()) // best-effort: no error, no panic

	if _, err := verify(v, s.sign(t, goodClaims(clk, idp.url()))); err == nil {
		t.Fatal("verified with no keys")
	}
	idp.set(func(p *fakeIdP) { p.failJWKS = false })
	clk.Add(30 * time.Second)
	if _, err := verify(v, s.sign(t, goodClaims(clk, idp.url()))); err != nil {
		t.Fatalf("did not recover: %v", err)
	}
}

// The discovery document must name the issuer we asked about.
func TestDiscovery_IssuerMismatchRejected(t *testing.T) {
	clk := newClock()
	s := rsSigner(t, "k1")
	idp := newIdP(t, s)
	idp.set(func(p *fakeIdP) { p.discIssuer = "https://evil.example.com" })
	v := newVerifier(t, clk, idp)
	if _, err := verify(v, s.sign(t, goodClaims(clk, idp.url()))); err == nil {
		t.Fatal("keys from a discovery document naming another issuer were trusted")
	}
	if _, j := idp.hits(); j != 0 {
		t.Fatalf("jwks fetched (%d) despite the mismatched discovery document", j)
	}
}

// Several issuers are independent: each has its own audience, keys and throttle.
func TestVerify_MultipleIssuers(t *testing.T) {
	clk := newClock()
	a, b := rsSigner(t, "ka"), esSigner(t, "kb")
	idpA, idpB := newIdP(t, a), newIdP(t, b)
	cfg := config.OIDC{Enabled: true, Issuers: []config.OIDCIssuer{
		{Issuer: idpA.url(), Audience: "client-a"},
		{Issuer: idpB.url(), Audience: "client-b", Mapping: config.MappingAutoProvision},
	}}
	v, err := oidc.New(cfg, oidc.WithClock(clk.Now), oidc.WithHTTPClient(idpA.srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	ca, cb := goodClaims(clk, idpA.url()), goodClaims(clk, idpB.url())
	ca["aud"], cb["aud"] = "client-a", "client-b"
	if _, err := verify(v, a.sign(t, ca)); err != nil {
		t.Fatalf("issuer A: %v", err)
	}
	if _, err := verify(v, b.sign(t, cb)); err != nil {
		t.Fatalf("issuer B: %v", err)
	}
	// A's audience does not carry over to B, nor B's key to A.
	cb["aud"] = "client-a"
	if _, err := verify(v, b.sign(t, cb)); err == nil {
		t.Fatal("issuer B accepted issuer A's audience")
	}
	cx := goodClaims(clk, idpA.url())
	cx["aud"] = "client-a"
	if _, err := verify(v, b.sign(t, cx)); err == nil {
		t.Fatal("a token signed by B's key verified as issuer A")
	}
}

// Issuer URL policy at construction: https, or http only for loopback or with
// the explicit per-issuer flag.
func TestNew_IssuerPolicy(t *testing.T) {
	mk := func(iss string, insecure bool) error {
		_, err := oidc.New(config.OIDC{Enabled: true, Issuers: []config.OIDCIssuer{{Issuer: iss, Audience: "a", AllowInsecureIssuer: insecure}}})

		return err
	}
	if err := mk("http://idp.example.com", false); err == nil {
		t.Fatal("http issuer on a public host accepted without the flag")
	}
	if err := mk("http://dex.localtest.me:5556/dex", false); err == nil {
		t.Fatal("localtest.me is not a loopback literal and needs allow_insecure_issuer")
	}
	for _, ok := range []struct {
		iss      string
		insecure bool
	}{{"https://idp.example.com", false}, {"http://127.0.0.1:5556", false}, {"http://localhost:5556/dex", false}, {"http://dex.localtest.me:5556/dex", true}} {
		if err := mk(ok.iss, ok.insecure); err != nil {
			t.Errorf("%q (insecure=%v): %v", ok.iss, ok.insecure, err)
		}
	}
	if _, err := oidc.New(config.OIDC{}); err == nil {
		t.Fatal("a disabled config must not build a verifier")
	}
	if _, err := oidc.New(config.OIDC{Enabled: true}); err == nil {
		t.Fatal("no issuers must not build a verifier")
	}
}

// A discovery/JWKS URL that downgrades to plain http on a non-loopback host is
// refused even when the issuer itself passed the policy.
func TestDiscovery_JWKSURIFollowsIssuerPolicy(t *testing.T) {
	clk := newClock()
	s := rsSigner(t, "k1")
	idp := newIdP(t, s)
	idp.set(func(p *fakeIdP) { p.jwksURI = "http://idp.example.com/keys" })
	v := newVerifier(t, clk, idp)
	if _, err := verify(v, s.sign(t, goodClaims(clk, idp.url()))); err == nil {
		t.Fatal("jwks_uri on plain http to a public host was followed")
	}
}
