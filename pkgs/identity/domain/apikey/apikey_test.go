package apikey_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/apikey"
)

var (
	now    = time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	secret = "xk_" + strings.Repeat("a", 43)
)

func TestNew(t *testing.T) {
	k, err := apikey.New("key-1", "acc-1", "ci", secret, time.Time{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if k.ID() != "key-1" || k.ID().String() != "key-1" || k.AccountID() != "acc-1" || k.Name() != "ci" || k.Status() != apikey.StatusActive || k.Version() != 0 {
		t.Fatalf("key = %+v", k)
	}
	if k.Prefix() != secret[:apikey.PrefixLen] || k.Hash() != apikey.HashSecret(secret) || !k.CreatedAt().Equal(now) || !k.ExpiresAt().IsZero() || !k.RevokedAt().IsZero() {
		t.Fatalf("key = %+v", k)
	}
	if err := k.Usable(now.Add(100 * 365 * 24 * time.Hour)); err != nil {
		t.Fatalf("never expires: %v", err)
	}

	exp := now.Add(time.Hour)
	k, err = apikey.New("key-2", "acc-1", "short-lived", secret, exp, now)
	if err != nil || !k.ExpiresAt().Equal(exp) {
		t.Fatalf("with expiry: %v, %+v", err, k)
	}
	if err := k.Usable(exp.Add(-time.Second)); err != nil {
		t.Fatalf("before expiry: %v", err)
	}
	if err := k.Usable(exp); !errors.Is(err, apikey.ErrExpired) {
		t.Fatalf("at expiry: %v", err)
	}

	for name, tc := range map[string]struct {
		id        apikey.ID
		account   string
		keyName   string
		secret    string
		expiresAt time.Time
		want      error
	}{
		"empty id":      {"", "acc", "n", secret, time.Time{}, apikey.ErrInvalidID},
		"empty account": {"k", "", "n", secret, time.Time{}, apikey.ErrInvalidAccountID},
		"empty name":    {"k", "acc", "", secret, time.Time{}, apikey.ErrInvalidName},
		"short secret":  {"k", "acc", "n", "xk_short", time.Time{}, apikey.ErrInvalidSecret},
		"past expiry":   {"k", "acc", "n", secret, now, apikey.ErrInvalidExpiry},
	} {
		if _, err := apikey.New(tc.id, tc.account, tc.keyName, tc.secret, tc.expiresAt, now); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestReconstitute(t *testing.T) {
	p := apikey.ReconstituteParams{
		ID: "key-1", AccountID: "acc-1", Name: "ci", Prefix: "xk_aaaaa", Hash: apikey.HashSecret(secret),
		Status: apikey.StatusRevoked, Version: 2, CreatedAt: now, ExpiresAt: now.Add(time.Hour), RevokedAt: now.Add(time.Minute),
	}
	k, err := apikey.Reconstitute(p)
	if err != nil || k.Version() != 2 || k.Status() != apikey.StatusRevoked || !k.RevokedAt().Equal(now.Add(time.Minute)) || k.Prefix() != "xk_aaaaa" || k.Hash() != p.Hash {
		t.Fatalf("reconstitute = %+v, %v", k, err)
	}
	if err := k.Usable(now); !errors.Is(err, apikey.ErrRevoked) {
		t.Fatalf("revoked usable: %v", err)
	}
	for name, tc := range map[string]struct {
		mut  func(*apikey.ReconstituteParams)
		want error
	}{
		"empty id":      {func(p *apikey.ReconstituteParams) { p.ID = "" }, apikey.ErrInvalidID},
		"empty account": {func(p *apikey.ReconstituteParams) { p.AccountID = "" }, apikey.ErrInvalidAccountID},
		"empty name":    {func(p *apikey.ReconstituteParams) { p.Name = "" }, apikey.ErrInvalidName},
		"bad status":    {func(p *apikey.ReconstituteParams) { p.Status = 0 }, apikey.ErrInvalidStatus},
	} {
		q := p
		tc.mut(&q)
		if _, err := apikey.Reconstitute(q); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestRevoke(t *testing.T) {
	k, _ := apikey.New("key-1", "acc-1", "ci", secret, time.Time{}, now)
	later := now.Add(time.Minute)
	if err := k.Revoke(later); err != nil || k.Status() != apikey.StatusRevoked || !k.RevokedAt().Equal(later) {
		t.Fatalf("revoke: %v, %+v", err, k)
	}
	if err := k.Usable(later); !errors.Is(err, apikey.ErrRevoked) {
		t.Fatalf("usable after revoke: %v", err)
	}
	if err := k.Revoke(later); !errors.Is(err, apikey.ErrAlreadyRevoked) {
		t.Fatalf("revoke twice: %v", err)
	}
}

func TestStatus(t *testing.T) {
	for _, s := range []apikey.Status{apikey.StatusActive, apikey.StatusRevoked} {
		got, err := apikey.ParseStatus(s.String())
		if err != nil || got != s {
			t.Fatalf("ParseStatus(%q) = %v, %v", s, got, err)
		}
	}
	if apikey.Status(0).String() != "UNKNOWN" {
		t.Fatal("zero status must print UNKNOWN")
	}
	if _, err := apikey.ParseStatus("nope"); !errors.Is(err, apikey.ErrInvalidStatus) {
		t.Fatalf("unknown: %v", err)
	}
}
