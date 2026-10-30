package invite_test

import (
	"errors"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/invite"
)

var now = time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)

func TestNew(t *testing.T) {
	i, err := invite.New("inv-1", "  Ops@Example.COM ", "acc-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if i.ID() != "inv-1" || i.ID().String() != "inv-1" || i.Email() != "ops@example.com" || i.AccountID() != "acc-1" || i.Version() != 0 {
		t.Fatalf("invite = %+v", i)
	}
	if !i.CreatedAt().Equal(now) || !i.ExpiresAt().Equal(now.Add(7*24*time.Hour)) || !i.ClaimedAt().IsZero() {
		t.Fatalf("times: created %v expires %v claimed %v", i.CreatedAt(), i.ExpiresAt(), i.ClaimedAt())
	}

	for name, tc := range map[string]struct {
		id        invite.ID
		email     string
		accountID string
		want      error
	}{
		"empty id":        {"", "a@b.co", "acc", invite.ErrInvalidID},
		"empty email":     {"i", " ", "acc", invite.ErrInvalidEmail},
		"no at":           {"i", "ab.co", "acc", invite.ErrInvalidEmail},
		"two ats":         {"i", "a@b@c.co", "acc", invite.ErrInvalidEmail},
		"empty local":     {"i", "@b.co", "acc", invite.ErrInvalidEmail},
		"empty domain":    {"i", "a@", "acc", invite.ErrInvalidEmail},
		"inner space":     {"i", "a b@c.co", "acc", invite.ErrInvalidEmail},
		"empty account":   {"i", "a@b.co", "", invite.ErrInvalidAccount},
		"display address": {"i", "Ops <a@b.co>", "acc", invite.ErrInvalidEmail},
	} {
		if _, err := invite.New(tc.id, tc.email, tc.accountID, now); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestClaim(t *testing.T) {
	pending := func() *invite.Invite {
		i, err := invite.New("inv-1", "ops@example.com", "acc-1", now)
		if err != nil {
			t.Fatal(err)
		}
		return i
	}

	i := pending()
	// The email compares case-insensitively (IdPs differ on case).
	if err := i.Claim(" OPS@example.com", true, now.Add(time.Hour)); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !i.ClaimedAt().Equal(now.Add(time.Hour)) {
		t.Fatalf("claimed at %v", i.ClaimedAt())
	}
	// Single use.
	if err := i.Claim("ops@example.com", true, now.Add(2*time.Hour)); !errors.Is(err, invite.ErrAlreadyClaimed) {
		t.Fatalf("second claim = %v, want already claimed", err)
	}

	// The last instant is still valid; expiry is exclusive of the boundary the other way.
	if err := pending().Claim("ops@example.com", true, now.Add(invite.TTL-time.Nanosecond)); err != nil {
		t.Fatalf("claim just before expiry: %v", err)
	}
	if err := pending().Claim("ops@example.com", true, now.Add(invite.TTL)); !errors.Is(err, invite.ErrExpired) {
		t.Fatalf("claim at expiry = %v, want expired", err)
	}
	if err := pending().Claim("ops@example.com", false, now); !errors.Is(err, invite.ErrEmailUnverified) {
		t.Fatalf("unverified email = %v", err)
	}
	if err := pending().Claim("other@example.com", true, now); !errors.Is(err, invite.ErrEmailMismatch) {
		t.Fatalf("other email = %v", err)
	}
	// An unverified claim does not spend the invite.
	j := pending()
	_ = j.Claim("ops@example.com", false, now)
	if !j.ClaimedAt().IsZero() {
		t.Fatal("a refused claim must leave the invite pending")
	}
	// An unparsable token email is a mismatch, not a crash.
	if err := pending().Claim("not-an-email", true, now); !errors.Is(err, invite.ErrEmailMismatch) {
		t.Fatalf("garbage email = %v", err)
	}
}

func TestReconstitute(t *testing.T) {
	p := invite.ReconstituteParams{ID: "inv-1", Email: "ops@example.com", AccountID: "acc-1", CreatedAt: now, ExpiresAt: now.Add(invite.TTL), ClaimedAt: now.Add(time.Hour), Version: 3}
	i, err := invite.Reconstitute(p)
	if err != nil || i.Version() != 3 || !i.ClaimedAt().Equal(now.Add(time.Hour)) || i.Email() != "ops@example.com" {
		t.Fatalf("reconstitute = %+v, %v", i, err)
	}
	for name, mut := range map[string]func(*invite.ReconstituteParams){
		"id":      func(p *invite.ReconstituteParams) { p.ID = "" },
		"email":   func(p *invite.ReconstituteParams) { p.Email = "nope" },
		"account": func(p *invite.ReconstituteParams) { p.AccountID = "" },
	} {
		q := p
		mut(&q)
		if _, err := invite.Reconstitute(q); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestNormalizeEmail(t *testing.T) {
	if got, err := invite.NormalizeEmail("  A@B.Co "); err != nil || got != "a@b.co" {
		t.Fatalf("normalize = %q, %v", got, err)
	}
	if _, err := invite.NormalizeEmail("x"); !errors.Is(err, invite.ErrInvalidEmail) {
		t.Fatalf("bad email = %v", err)
	}
}
