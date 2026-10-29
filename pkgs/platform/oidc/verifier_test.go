package oidc_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
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
		// iat from the future is clock skew up to 60 s, a forged token beyond.
		"iat 59s ahead": {func(c jwt.MapClaims) { c["iat"] = now.Add(59 * time.Second).Unix() }, true},
		"iat 61s ahead": {func(c jwt.MapClaims) { c["iat"] = now.Add(61 * time.Second).Unix() }, false},
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

	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

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
		"ES384 is not on the allow-list":          signWith(jwt.SigningMethodES384, p384, "k-es"),
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
	t.Run("RSA key below 2048 bits", func(t *testing.T) {
		clk := newClock()
		small, err := rsa.GenerateKey(rand.Reader, 1024)
		if err != nil {
			t.Fatal(err)
		}
		weak := &signer{kid: "k-weak", alg: "RS256", method: jwt.SigningMethodRS256, priv: small, jwk: map[string]any{
			"kty": "RSA", "kid": "k-weak", "use": "sig", "alg": "RS256",
			"n": b64(small.N.Bytes()), "e": b64(big.NewInt(int64(small.E)).Bytes()),
		}}
		idp := newIdP(t, weak)
		v := newVerifier(t, clk, idp)
		if _, err := verify(v, weak.sign(t, goodClaims(clk, idp.url()))); err == nil {
			t.Fatal("a 1024-bit RSA key was trusted")
		}
	})
	t.Run("token without kid", func(t *testing.T) {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims())
		out, _ := tok.SignedString(rsaKey)
		if _, err := verify(v, out); err == nil {
			t.Fatal("a token without kid was accepted")
		}
	})
	t.Run("unsupported key types are skipped", func(t *testing.T) {
		clk := newClock()
		good := rsSigner(t, "k-good")
		idp := newIdP(t)
		idp.rawKeys(map[string]any{"kty": "oct", "kid": "k-oct", "k": "AAAA"}, good.jwk)
		v := newVerifier(t, clk, idp)
		if _, err := verify(v, good.sign(t, goodClaims(clk, idp.url()))); err != nil {
			t.Fatalf("good key rejected beside an unsupported one: %v", err)
		}
	})
}

// Intervals are fixed: JWKS refresh hourly, one unknown-kid refetch per issuer
// per 30 s, keys good for 24 h past the last success.
func TestJWKS_FixedIntervals(t *testing.T) {
	if oidc.DefaultRefreshInterval != time.Hour || oidc.DefaultUnknownKIDInterval != 30*time.Second || oidc.MaxStale != 24*time.Hour {
		t.Fatalf("intervals: refresh=%v unknown-kid=%v stale=%v", oidc.DefaultRefreshInterval, oidc.DefaultUnknownKIDInterval, oidc.MaxStale)
	}
}

// The keys are refreshed on a timer, whatever the IdP's Cache-Control says.
func TestJWKS_PeriodicRefresh(t *testing.T) {
	clk := newClock()
	s := rsSigner(t, "k1")
	idp := newIdP(t, s)
	idp.set(func(p *fakeIdP) { p.cacheControl = "max-age=999999" })
	v := newVerifier(t, clk, idp, oidc.WithRefreshInterval(30*time.Millisecond))
	if _, err := verify(v, s.sign(t, goodClaims(clk, idp.url()))); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "periodic JWKS refreshes", func() bool { _, j := idp.hits(); return j >= 4 })
	if d, _ := idp.hits(); d != 1 {
		t.Fatalf("discovery ran %d times, want once (the jwks_uri is remembered)", d)
	}
}

// An unknown kid refetches at once, but at most once per window per issuer; a
// kid that is still unknown afterwards is rejected, never guessed.
func TestJWKS_UnknownKidRefetchAndThrottle(t *testing.T) {
	const window = 400 * time.Millisecond
	clk := newClock()
	old, fresh, ghost := rsSigner(t, "k-old"), esSigner(t, "k-new"), edSigner(t, "k-ghost")
	idp := newIdP(t, old)
	v := newVerifier(t, clk, idp, oidc.WithUnknownKIDInterval(window))
	if _, err := verify(v, old.sign(t, goodClaims(clk, idp.url()))); err != nil {
		t.Fatal(err)
	}

	idp.setKeys(old, fresh) // the IdP rotates
	if _, err := verify(v, fresh.sign(t, goodClaims(clk, idp.url()))); err != nil {
		t.Fatalf("a rotated-in kid must be picked up at once: %v", err)
	}
	if _, j := idp.hits(); j != 2 {
		t.Fatalf("fetches = %d, want 2", j)
	}

	if _, err := verify(v, ghost.sign(t, goodClaims(clk, idp.url()))); err == nil {
		t.Fatal("unknown kid accepted")
	}
	if _, j := idp.hits(); j != 2 {
		t.Fatalf("unknown kid inside the window refetched (%d fetches)", j)
	}

	idp.setKeys(old, fresh, ghost) // upstream has it now, but the window is still closed
	if _, err := verify(v, ghost.sign(t, goodClaims(clk, idp.url()))); err == nil {
		t.Fatal("throttle bypassed")
	}
	if _, j := idp.hits(); j != 2 {
		t.Fatalf("throttled lookups fetched (%d fetches)", j)
	}

	time.Sleep(window + 50*time.Millisecond)
	if _, err := verify(v, ghost.sign(t, goodClaims(clk, idp.url()))); err != nil {
		t.Fatalf("after the window the refetch must happen: %v", err)
	}
	if _, j := idp.hits(); j != 3 {
		t.Fatalf("fetches = %d, want 3", j)
	}
}

// Many requests carrying a freshly rotated kid cost one fetch; a flood of
// tokens with a kid nobody has costs at most one more per window.
func TestJWKS_ConcurrentLookupsShareFetches(t *testing.T) {
	clk := newClock()
	old, fresh := rsSigner(t, "k-old"), esSigner(t, "k-new")
	idp := newIdP(t, old)
	v := newVerifier(t, clk, idp)
	if _, err := verify(v, old.sign(t, goodClaims(clk, idp.url()))); err != nil {
		t.Fatal(err)
	}
	idp.setKeys(old, fresh)

	tok := fresh.sign(t, goodClaims(clk, idp.url()))
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for range 50 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := verify(v, tok); errs <- err }()
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

	// Now a flood of bogus kids: the window was consumed by the rotation above,
	// so nothing more is fetched.
	ghost := edSigner(t, "k-ghost").sign(t, goodClaims(clk, idp.url()))
	for range 50 {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = verify(v, ghost) }()
	}
	wg.Wait()
	if _, j := idp.hits(); j != 2 {
		t.Fatalf("bogus kids caused fetches: %d, want 2", j)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// jwkset keeps the old keys when a refresh fails (checked here, not assumed),
// but has no bound on how long; the adapter caps it at 24 h after the last
// successful fetch. An unknown kid is rejected throughout.
func TestJWKS_StaleIfError(t *testing.T) {
	clk := newClock()
	s := rsSigner(t, "k1")
	idp := newIdP(t, s)
	v := newVerifier(t, clk, idp, oidc.WithRefreshInterval(30*time.Millisecond), oidc.WithUnknownKIDInterval(50*time.Millisecond))
	tok := func() string { return s.sign(t, goodClaims(clk, idp.url())) }
	if _, err := verify(v, tok()); err != nil {
		t.Fatal(err)
	}

	idp.set(func(p *fakeIdP) { p.failJWKS = true })
	_, base := idp.hits()
	waitFor(t, "failed refreshes", func() bool { _, j := idp.hits(); return j >= base+3 })
	if _, err := verify(v, tok()); err != nil {
		t.Fatalf("after failed refreshes the old key must keep verifying: %v", err)
	}
	if _, err := verify(v, esSigner(t, "k-unknown").sign(t, goodClaims(clk, idp.url()))); err == nil {
		t.Fatal("an unknown kid was accepted while serving stale keys")
	}

	clk.Add(24*time.Hour - time.Minute) // inside 24 h of the last success
	if _, err := verify(v, tok()); err != nil {
		t.Fatalf("inside 24 h of the last success the stale key must still verify: %v", err)
	}
	clk.Add(2 * time.Minute) // 24h01m after the last success
	if _, err := verify(v, tok()); err == nil {
		t.Fatal("a key older than 24 h without a successful refresh was still trusted")
	}

	// Recovery: the next successful refresh brings the keys back.
	idp.set(func(p *fakeIdP) { p.failJWKS = false })
	waitFor(t, "recovery", func() bool { _, err := verify(v, tok()); return err == nil })
}

// A JWKS that answers 200 but that jwkset refuses (here a malformed RSA key)
// leaves the old keys in place and must not count as a refresh: otherwise an
// IdP serving junk would keep stale keys alive past the 24 h bound.
func TestJWKS_RefusedContentDoesNotExtendTrust(t *testing.T) {
	clk := newClock()
	s := rsSigner(t, "k1")
	idp := newIdP(t, s)
	v := newVerifier(t, clk, idp, oidc.WithRefreshInterval(30*time.Millisecond))
	tok := func() string { return s.sign(t, goodClaims(clk, idp.url())) }
	if _, err := verify(v, tok()); err != nil {
		t.Fatal(err)
	}

	idp.rawKeys(s.jwk, map[string]any{"kty": "RSA", "kid": "k-broken", "n": "!!", "e": "AQAB"})
	_, base := idp.hits()
	waitFor(t, "refreshes of the refused JWKS", func() bool { _, j := idp.hits(); return j >= base+3 })
	if _, err := verify(v, tok()); err != nil {
		t.Fatalf("old keys must keep working when a refresh is refused: %v", err)
	}
	// 25 h later the IdP is still serving the refused JWKS and refreshes keep
	// "succeeding" at the HTTP level; they must not have renewed the trust.
	clk.Add(25 * time.Hour)
	_, base = idp.hits()
	waitFor(t, "refreshes after the clock moved", func() bool { _, j := idp.hits(); return j >= base+3 })
	if _, err := verify(v, tok()); err == nil {
		t.Fatal("refused refreshes extended the trust in stale keys past 24 h")
	}
}

// An IdP that is down at startup is not fatal: New and Warm succeed, requests
// fail closed, and the keys arrive once the IdP answers.
func TestJWKS_StartupFailureIsNotFatal(t *testing.T) {
	t.Run("jwks down", func(t *testing.T) {
		clk := newClock()
		s := rsSigner(t, "k1")
		idp := newIdP(t, s)
		idp.set(func(p *fakeIdP) { p.failJWKS = true })
		v := newVerifier(t, clk, idp, oidc.WithRefreshInterval(30*time.Millisecond))
		v.Warm(t.Context())
		if _, err := verify(v, s.sign(t, goodClaims(clk, idp.url()))); err == nil {
			t.Fatal("verified with no keys")
		}
		idp.set(func(p *fakeIdP) { p.failJWKS = false })
		waitFor(t, "recovery", func() bool { _, err := verify(v, s.sign(t, goodClaims(clk, idp.url()))); return err == nil })
	})
	t.Run("discovery down", func(t *testing.T) {
		clk := newClock()
		s := rsSigner(t, "k1")
		idp := newIdP(t, s)
		idp.set(func(p *fakeIdP) { p.failDiscovery = true })
		v := newVerifier(t, clk, idp)
		v.Warm(t.Context())
		tok := s.sign(t, goodClaims(clk, idp.url()))
		if _, err := verify(v, tok); err == nil {
			t.Fatal("verified with no discovery")
		}
		idp.set(func(p *fakeIdP) { p.failDiscovery = false })
		if d, _ := idp.hits(); d != 1 {
			t.Fatalf("discovery attempts = %d, want 1 so far", d)
		}
		if _, err := verify(v, tok); err == nil { // same 30 s window: no new attempt
			t.Fatal("verified inside the discovery window")
		}
		if d, _ := idp.hits(); d != 1 {
			t.Fatalf("discovery retried inside its 30 s window (%d attempts)", d)
		}
		clk.Add(30 * time.Second)
		if _, err := verify(v, s.sign(t, goodClaims(clk, idp.url()))); err != nil {
			t.Fatalf("did not recover after the window: %v", err)
		}
	})
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
