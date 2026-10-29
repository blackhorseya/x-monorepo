package account_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
)

var now = time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)

func TestNew(t *testing.T) {
	a, err := account.New("acc-1", "  ops  ", []string{" admin", "ops", "admin"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID() != "acc-1" || a.ID().String() != "acc-1" || a.Name() != "ops" || a.Status() != account.StatusActive || !a.IsActive() || a.Version() != 0 {
		t.Fatalf("account = %+v", a)
	}
	if !slices.Equal(a.Roles(), []string{"admin", "ops"}) || !a.HasRole("admin") || a.HasRole("root") {
		t.Fatalf("roles = %v", a.Roles())
	}
	if !a.CreatedAt().Equal(now) || !a.UpdatedAt().Equal(now) {
		t.Fatalf("times = %v %v", a.CreatedAt(), a.UpdatedAt())
	}
	a.Roles()[0] = "mutated"
	if a.Roles()[0] != "admin" {
		t.Fatal("Roles() must return a copy")
	}
	if a, err := account.New("acc-2", "no roles", nil, now); err != nil || len(a.Roles()) != 0 {
		t.Fatalf("no roles: %v, %v", a, err)
	}

	for name, tc := range map[string]struct {
		id    account.ID
		name  string
		roles []string
		want  error
	}{
		"empty id":   {"", "x", nil, account.ErrInvalidID},
		"empty name": {"a", "  ", nil, account.ErrInvalidName},
		"empty role": {"a", "x", []string{"admin", " "}, account.ErrInvalidRole},
	} {
		if _, err := account.New(tc.id, tc.name, tc.roles, now); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestReconstitute(t *testing.T) {
	p := account.ReconstituteParams{ID: "acc-1", Name: "ops", Roles: []string{"ops", "admin"}, Status: account.StatusDisabled, Version: 3, CreatedAt: now, UpdatedAt: now.Add(time.Hour)}
	a, err := account.Reconstitute(p)
	if err != nil || a.Version() != 3 || a.Status() != account.StatusDisabled || !slices.Equal(a.Roles(), []string{"admin", "ops"}) || !a.UpdatedAt().Equal(now.Add(time.Hour)) {
		t.Fatalf("reconstitute = %+v, %v", a, err)
	}
	for name, tc := range map[string]struct {
		mut  func(*account.ReconstituteParams)
		want error
	}{
		"empty id":   {func(p *account.ReconstituteParams) { p.ID = "" }, account.ErrInvalidID},
		"empty name": {func(p *account.ReconstituteParams) { p.Name = "" }, account.ErrInvalidName},
		"bad status": {func(p *account.ReconstituteParams) { p.Status = 0 }, account.ErrInvalidStatus},
		"empty role": {func(p *account.ReconstituteParams) { p.Roles = []string{""} }, account.ErrInvalidRole},
	} {
		q := p
		tc.mut(&q)
		if _, err := account.Reconstitute(q); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestDisableEnable(t *testing.T) {
	a, _ := account.New("acc-1", "ops", nil, now)
	later := now.Add(time.Minute)
	if err := a.Enable(later); !errors.Is(err, account.ErrAlreadyActive) {
		t.Fatalf("enable active: %v", err)
	}
	if err := a.Disable(later); err != nil || a.IsActive() || a.Status() != account.StatusDisabled || !a.UpdatedAt().Equal(later) {
		t.Fatalf("disable: %v, %+v", err, a)
	}
	if err := a.Disable(later); !errors.Is(err, account.ErrAlreadyDisabled) {
		t.Fatalf("disable twice: %v", err)
	}
	if err := a.Enable(later.Add(time.Minute)); err != nil || !a.IsActive() || !a.UpdatedAt().Equal(later.Add(time.Minute)) {
		t.Fatalf("enable: %v, %+v", err, a)
	}
}

func TestAssignRoles(t *testing.T) {
	a, _ := account.New("acc-1", "ops", []string{"ops"}, now)
	later := now.Add(time.Minute)
	if err := a.AssignRoles([]string{"admin", "", "ops"}, later); !errors.Is(err, account.ErrInvalidRole) || !slices.Equal(a.Roles(), []string{"ops"}) {
		t.Fatalf("bad role: %v, roles = %v", err, a.Roles())
	}
	if err := a.AssignRoles([]string{"viewer", "admin"}, later); err != nil || !slices.Equal(a.Roles(), []string{"admin", "viewer"}) || !a.UpdatedAt().Equal(later) {
		t.Fatalf("assign: %v, %+v", err, a)
	}
	if err := a.AssignRoles(nil, later); err != nil || len(a.Roles()) != 0 {
		t.Fatalf("clear: %v, %v", err, a.Roles())
	}
}

func TestStatus(t *testing.T) {
	for _, s := range []account.Status{account.StatusActive, account.StatusDisabled} {
		got, err := account.ParseStatus(s.String())
		if err != nil || got != s {
			t.Fatalf("ParseStatus(%q) = %v, %v", s, got, err)
		}
	}
	if account.Status(0).String() != "UNKNOWN" {
		t.Fatal("zero status must print UNKNOWN")
	}
	if _, err := account.ParseStatus("nope"); !errors.Is(err, account.ErrInvalidStatus) {
		t.Fatalf("unknown: %v", err)
	}
}

func TestRevokeSessions(t *testing.T) {
	a, _ := account.New("acc-1", "ops", []string{"admin"}, now)
	if !a.SessionsRevokedAt().IsZero() {
		t.Fatalf("a new account has never revoked: %v", a.SessionsRevokedAt())
	}
	later := now.Add(time.Hour)
	if err := a.RevokeSessions(later); err != nil {
		t.Fatal(err)
	}
	if !a.SessionsRevokedAt().Equal(later) || !a.UpdatedAt().Equal(later) {
		t.Fatalf("revoked at %v updated at %v, want %v", a.SessionsRevokedAt(), a.UpdatedAt(), later)
	}
	// It touches nothing else: status and roles keep standing.
	if !a.IsActive() || !a.HasRole("admin") {
		t.Fatalf("RevokeSessions must not disable or strip roles: %+v", a)
	}
	// A second revocation moves the line forward, never back.
	if err := a.RevokeSessions(now); err != nil || !a.SessionsRevokedAt().Equal(later) {
		t.Fatalf("an earlier time must not move the line back: %v %v", err, a.SessionsRevokedAt())
	}

	r, err := account.Reconstitute(account.ReconstituteParams{ID: "acc-2", Name: "x", Status: account.StatusActive, CreatedAt: now, UpdatedAt: now, SessionsRevokedAt: later})
	if err != nil || !r.SessionsRevokedAt().Equal(later) {
		t.Fatalf("reconstitute keeps sessions_revoked_at: %v %v", r, err)
	}
}

func TestCheckSession(t *testing.T) {
	revoked := now
	for name, tc := range map[string]struct {
		revokedAt, issuedAt time.Time
		want                error
	}{
		"never revoked":            {time.Time{}, now.Add(-24 * time.Hour), nil},
		"never revoked, no iat":    {time.Time{}, time.Time{}, account.ErrMissingIssuedAt},
		"issued before revocation": {revoked, now.Add(-time.Second), account.ErrSessionRevoked},
		"issued at revocation":     {revoked, now, nil}, // "earlier than" is the rule: a re-login at that very second passes
		"issued after revocation":  {revoked, now.Add(time.Second), nil},
		"revoked, no iat":          {revoked, time.Time{}, account.ErrMissingIssuedAt},
	} {
		if err := account.CheckSession(tc.revokedAt, tc.issuedAt); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}
