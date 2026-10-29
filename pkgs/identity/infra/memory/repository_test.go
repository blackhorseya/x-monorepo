package memory_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/apikey"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/infra/memory"
)

var now = time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)

func TestAccountRepository(t *testing.T) {
	c := context.Background()
	repo := memory.NewAccountRepository()

	if _, err := repo.FindByID(c, "a"); !errors.Is(err, account.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	a, _ := account.New("a", "ops", []string{"admin"}, now)
	if err := repo.Add(c, a); err != nil {
		t.Fatal(err)
	}
	if err := repo.Add(c, a); !errors.Is(err, account.ErrDuplicate) {
		t.Fatalf("duplicate: %v", err)
	}
	loaded, err := repo.FindByID(c, "a")
	if err != nil || loaded.Version() != 1 || !loaded.HasRole("admin") {
		t.Fatalf("loaded = %+v, %v", loaded, err)
	}
	other, _ := repo.FindByID(c, "a")
	_ = loaded.Disable(now)
	if err := repo.Save(c, loaded); err != nil {
		t.Fatal(err)
	}
	if err := repo.Save(c, other); !errors.Is(err, account.ErrStale) {
		t.Fatalf("stale: %v", err)
	}
	if err := repo.Save(c, a); !errors.Is(err, account.ErrStale) {
		t.Fatalf("never-loaded save: %v", err)
	}
	reloaded, _ := repo.FindByID(c, "a")
	if reloaded.Version() != 2 || reloaded.IsActive() {
		t.Fatalf("reloaded = %+v", reloaded)
	}

	for _, id := range []account.ID{"c", "b"} {
		x, _ := account.New(id, "n", nil, now)
		_ = repo.Add(c, x)
	}
	page, next, err := repo.List(c, account.ListParams{Limit: 2})
	if err != nil || len(page) != 2 || page[0].ID() != "a" || page[1].ID() != "b" || next != "b" {
		t.Fatalf("page 1 = %v, %q, %v", page, next, err)
	}
	page, next, err = repo.List(c, account.ListParams{Limit: 2, After: next})
	if err != nil || len(page) != 1 || page[0].ID() != "c" || next != "" {
		t.Fatalf("page 2 = %v, %q, %v", page, next, err)
	}

	cancelled, cancel := context.WithCancel(c)
	cancel()
	if _, err := repo.FindByID(cancelled, "a"); err == nil {
		t.Fatal("cancelled find")
	}
	if err := repo.Add(cancelled, a); err == nil {
		t.Fatal("cancelled add")
	}
	if err := repo.Save(cancelled, a); err == nil {
		t.Fatal("cancelled save")
	}
	if _, _, err := repo.List(cancelled, account.ListParams{}); err == nil {
		t.Fatal("cancelled list")
	}
}

func TestAPIKeyRepository(t *testing.T) {
	c := context.Background()
	repo := memory.NewAPIKeyRepository()
	secret := "xk_" + strings.Repeat("a", 43)

	if _, err := repo.FindByID(c, "k"); !errors.Is(err, apikey.ErrNotFound) {
		t.Fatalf("missing id: %v", err)
	}
	if _, err := repo.FindByHash(c, apikey.HashSecret(secret)); !errors.Is(err, apikey.ErrNotFound) {
		t.Fatalf("missing hash: %v", err)
	}
	k, _ := apikey.New("k1", "a", "laptop", secret, time.Time{}, now)
	if err := repo.Add(c, k); err != nil {
		t.Fatal(err)
	}
	if err := repo.Add(c, k); !errors.Is(err, apikey.ErrDuplicate) {
		t.Fatalf("duplicate id: %v", err)
	}
	sameSecret, _ := apikey.New("k9", "a", "twin", secret, time.Time{}, now)
	if err := repo.Add(c, sameSecret); !errors.Is(err, apikey.ErrDuplicate) {
		t.Fatalf("duplicate hash: %v", err)
	}
	byHash, err := repo.FindByHash(c, apikey.HashSecret(secret))
	if err != nil || byHash.ID() != "k1" || byHash.Version() != 1 {
		t.Fatalf("by hash = %+v, %v", byHash, err)
	}
	other, _ := repo.FindByID(c, "k1")
	_ = byHash.Revoke(now)
	if err := repo.Save(c, byHash); err != nil {
		t.Fatal(err)
	}
	if err := repo.Save(c, other); !errors.Is(err, apikey.ErrStale) {
		t.Fatalf("stale: %v", err)
	}
	reloaded, _ := repo.FindByID(c, "k1")
	if reloaded.Status() != apikey.StatusRevoked || reloaded.Version() != 2 {
		t.Fatalf("reloaded = %+v", reloaded)
	}

	k2, _ := apikey.New("k2", "a", "newer", "xk_"+strings.Repeat("b", 43), time.Time{}, now.Add(time.Second))
	k3, _ := apikey.New("k3", "a", "same-time", "xk_"+strings.Repeat("c", 43), time.Time{}, now.Add(time.Second))
	kb, _ := apikey.New("kb", "b", "other account", "xk_"+strings.Repeat("d", 43), time.Time{}, now)
	for _, x := range []*apikey.APIKey{k2, k3, kb} {
		if err := repo.Add(c, x); err != nil {
			t.Fatal(err)
		}
	}
	list, err := repo.ListByAccount(c, "a")
	if err != nil || len(list) != 3 || list[0].ID() != "k3" || list[1].ID() != "k2" || list[2].ID() != "k1" {
		t.Fatalf("list = %v, %v (want created desc, id desc)", list, err)
	}
	if list, _ := repo.ListByAccount(c, "nobody"); len(list) != 0 {
		t.Fatalf("list nobody = %v", list)
	}

	cancelled, cancel := context.WithCancel(c)
	cancel()
	if _, err := repo.FindByID(cancelled, "k1"); err == nil {
		t.Fatal("cancelled find")
	}
	if _, err := repo.FindByHash(cancelled, apikey.HashSecret(secret)); err == nil {
		t.Fatal("cancelled find by hash")
	}
	if err := repo.Add(cancelled, k); err == nil {
		t.Fatal("cancelled add")
	}
	if err := repo.Save(cancelled, k); err == nil {
		t.Fatal("cancelled save")
	}
	if _, err := repo.ListByAccount(cancelled, "a"); err == nil {
		t.Fatal("cancelled list")
	}
}

func TestAPIKeyRepository_TouchLastUsed(t *testing.T) {
	c := context.Background()
	repo := memory.NewAPIKeyRepository()
	k, _ := apikey.New("k1", "a", "laptop", "xk_"+strings.Repeat("a", 43), time.Time{}, now)
	if err := repo.Add(c, k); err != nil {
		t.Fatal(err)
	}
	window := time.Minute

	// Never used: the first touch writes.
	if ok, err := repo.TouchLastUsed(c, "k1", now, window); err != nil || !ok {
		t.Fatalf("first touch = %v, %v", ok, err)
	}
	// Inside the window: no write, last_used_at stays.
	if ok, err := repo.TouchLastUsed(c, "k1", now.Add(window-time.Second), window); err != nil || ok {
		t.Fatalf("touch inside window = %v, %v", ok, err)
	}
	if got, _ := repo.FindByID(c, "k1"); !got.LastUsedAt().Equal(now) {
		t.Fatalf("last_used_at = %v, want %v", got.LastUsedAt(), now)
	}
	// At the window's end it writes again.
	if ok, err := repo.TouchLastUsed(c, "k1", now.Add(window), window); err != nil || !ok {
		t.Fatalf("touch after window = %v, %v", ok, err)
	}
	if got, _ := repo.FindByID(c, "k1"); !got.LastUsedAt().Equal(now.Add(window)) {
		t.Fatalf("last_used_at = %v, want %v", got.LastUsedAt(), now.Add(window))
	}
	// A touch never moves the version, so it cannot make a revoke stale.
	loaded, _ := repo.FindByID(c, "k1")
	if _, err := repo.TouchLastUsed(c, "k1", now.Add(3*window), window); err != nil {
		t.Fatal(err)
	}
	_ = loaded.Revoke(now)
	if err := repo.Save(c, loaded); err != nil {
		t.Fatalf("save after touch: %v", err)
	}
	// Save writes the aggregate it loaded; it must not roll last_used_at back.
	if got, _ := repo.FindByID(c, "k1"); !got.LastUsedAt().Equal(now.Add(3 * window)) {
		t.Fatalf("save clobbered last_used_at: %v", got.LastUsedAt())
	}
	// An unknown id is nothing to write, not an error.
	if ok, err := repo.TouchLastUsed(c, "nope", now, window); err != nil || ok {
		t.Fatalf("unknown id = %v, %v", ok, err)
	}
	cancelled, cancel := context.WithCancel(c)
	cancel()
	if _, err := repo.TouchLastUsed(cancelled, "k1", now, window); err == nil {
		t.Fatal("cancelled touch")
	}
}
