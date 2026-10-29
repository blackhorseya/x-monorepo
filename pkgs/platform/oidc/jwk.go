package oidc

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
)

// Algorithms the verifier accepts, each tied to exactly one key type. Anything
// else — HS256, none, RS384/512, PS*, ES384/512 — is rejected before a key is
// even looked up.
const (
	algRS256 = "RS256"
	algES256 = "ES256"
	algEdDSA = "EdDSA"

	minRSABits = 2048
)

func algAllowed(alg string) bool {
	return alg == algRS256 || alg == algES256 || alg == algEdDSA
}

// jwk is one JSON Web Key as published in a JWKS.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// verifyKey is a parsed public key and the one algorithm it may verify.
type verifyKey struct {
	alg string
	key crypto.PublicKey
}

func b64Int(s string) (*big.Int, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) == 0 {
		return nil, errors.New("bad base64url integer")
	}

	return new(big.Int).SetBytes(b), nil
}

// parseJWK turns a JWK into a verification key. A key that is not for
// signatures, is of a kind we do not accept, is malformed, or declares an alg
// that does not fit its type is an error — the caller skips it, so one junk
// entry cannot take the others down.
func parseJWK(k jwk) (verifyKey, error) {
	if k.Use != "" && k.Use != "sig" {
		return verifyKey{}, fmt.Errorf("use %q is not sig", k.Use)
	}
	var vk verifyKey
	switch k.Kty {
	case "RSA":
		n, err := b64Int(k.N)
		if err != nil {
			return verifyKey{}, fmt.Errorf("rsa n: %w", err)
		}
		e, err := b64Int(k.E)
		if err != nil || !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 {
			return verifyKey{}, errors.New("rsa e is unusable")
		}
		if n.BitLen() < minRSABits {
			return verifyKey{}, fmt.Errorf("rsa key of %d bits is below %d", n.BitLen(), minRSABits)
		}
		vk = verifyKey{alg: algRS256, key: &rsa.PublicKey{N: n, E: int(e.Int64())}}
	case "EC":
		if k.Crv != "P-256" {
			return verifyKey{}, fmt.Errorf("ec curve %q is not P-256", k.Crv)
		}
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(x) != 32 {
			return verifyKey{}, errors.New("ec x is not a 32-byte coordinate")
		}
		y, err := base64.RawURLEncoding.DecodeString(k.Y)
		if err != nil || len(y) != 32 {
			return verifyKey{}, errors.New("ec y is not a 32-byte coordinate")
		}
		// ParseUncompressedPublicKey also checks the point is on the curve.
		pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
		if err != nil {
			return verifyKey{}, fmt.Errorf("ec point: %w", err)
		}
		vk = verifyKey{alg: algES256, key: pub}
	case "OKP":
		if k.Crv != "Ed25519" {
			return verifyKey{}, fmt.Errorf("okp curve %q is not Ed25519", k.Crv)
		}
		raw, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return verifyKey{}, errors.New("okp x is not an Ed25519 public key")
		}
		vk = verifyKey{alg: algEdDSA, key: ed25519.PublicKey(raw)}
	default:
		return verifyKey{}, fmt.Errorf("kty %q is not accepted", k.Kty)
	}
	if k.Alg != "" && k.Alg != vk.alg {
		return verifyKey{}, fmt.Errorf("alg %q does not fit a %s key", k.Alg, k.Kty)
	}

	return vk, nil
}

// parseJWKS reads a JWKS document into kid → key. Keys without a kid or that
// do not parse are skipped; a duplicate kid keeps neither (ambiguous), so a
// token naming it is rejected rather than checked against a guess.
func parseJWKS(body []byte) (map[string]verifyKey, error) {
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("jwks is not JSON: %w", err)
	}
	keys := make(map[string]verifyKey, len(doc.Keys))
	dup := map[string]bool{}
	for _, raw := range doc.Keys {
		var k jwk
		if json.Unmarshal(raw, &k) != nil || k.Kid == "" {
			continue
		}
		vk, err := parseJWK(k)
		if err != nil {
			continue
		}
		if _, seen := keys[k.Kid]; seen {
			dup[k.Kid] = true
		}
		keys[k.Kid] = vk
	}
	for kid := range dup {
		delete(keys, kid)
	}

	return keys, nil
}
