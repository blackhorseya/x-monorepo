package postgres_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/apikey"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

// Integration test: a throwaway Postgres container, or TEST_DATABASE_URL (see pgtest).
func TestRepositories_Postgres(t *testing.T) {
	url := pgtest.URL(t)

	c := context.Background()
	cfg := config.Postgres{URL: url}
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	db, cleanup, err := platformpg.NewDB(cfg, tel)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	m := postgres.Migration()
	// Down → Up exercises the .down.sql files too; start from whatever state the DB is in.
	for _, step := range []func(context.Context, config.Postgres, platformpg.Migration) error{platformpg.Up, platformpg.Down, platformpg.Up} {
		if err := step(c, cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	if v, dirty, err := platformpg.Version(c, cfg, m); err != nil || v != 2 || dirty {
		t.Fatalf("version = %d dirty=%v err=%v, want 2 clean", v, dirty, err)
	}
	if err := db.WithContext(c).Exec("TRUNCATE identity.accounts CASCADE").Error; err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	accounts := postgres.NewAccountRepository(db)
	keys := postgres.NewAPIKeyRepository(db)

	// ── accounts: add, reload (roles round-trip through jsonb), lock ──────
	if _, err := accounts.FindByID(c, "a"); !errors.Is(err, account.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	a, _ := account.New("a", "ops", []string{"ops", "admin"}, now)
	if err := accounts.Add(c, a); err != nil {
		t.Fatal(err)
	}
	if err := accounts.Add(c, a); !errors.Is(err, account.ErrDuplicate) {
		t.Fatalf("duplicate: %v", err)
	}
	loaded, err := accounts.FindByID(c, "a")
	if err != nil || loaded.Version() != 1 || !loaded.HasRole("admin") || !loaded.HasRole("ops") || len(loaded.Roles()) != 2 || !loaded.CreatedAt().Equal(now) {
		t.Fatalf("loaded = %+v, %v", loaded, err)
	}
	other, _ := accounts.FindByID(c, "a")
	if err := loaded.AssignRoles(nil, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := accounts.Save(c, loaded); err != nil {
		t.Fatal(err)
	}
	if err := accounts.Save(c, other); !errors.Is(err, account.ErrStale) {
		t.Fatalf("stale save: %v", err)
	}
	reloaded, _ := accounts.FindByID(c, "a")
	if reloaded.Version() != 2 || len(reloaded.Roles()) != 0 || !reloaded.UpdatedAt().Equal(now.Add(time.Second)) {
		t.Fatalf("reloaded = %+v", reloaded)
	}
	_ = reloaded.Disable(now)
	if err := accounts.Save(c, reloaded); err != nil {
		t.Fatal(err)
	}
	if again, _ := accounts.FindByID(c, "a"); again.IsActive() {
		t.Fatal("status did not persist")
	}
	// A status this code does not know is a corrupt row, not a crash.
	if err := db.WithContext(c).Exec("UPDATE identity.accounts SET status = 'WEIRD' WHERE id = 'a'").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.FindByID(c, "a"); !errors.Is(err, account.ErrInvalidStatus) {
		t.Fatalf("corrupt status: %v", err)
	}
	if err := db.WithContext(c).Exec("UPDATE identity.accounts SET status = 'ACTIVE' WHERE id = 'a'").Error; err != nil {
		t.Fatal(err)
	}

	for _, id := range []account.ID{"c", "b"} {
		x, _ := account.New(id, "n", nil, now)
		if err := accounts.Add(c, x); err != nil {
			t.Fatal(err)
		}
	}
	page, next, err := accounts.List(c, account.ListParams{Limit: 2})
	if err != nil || len(page) != 2 || page[0].ID() != "a" || page[1].ID() != "b" || next != "b" {
		t.Fatalf("page 1 = %v, %q, %v", page, next, err)
	}
	page, next, err = accounts.List(c, account.ListParams{Limit: 2, After: next})
	if err != nil || len(page) != 1 || page[0].ID() != "c" || next != "" {
		t.Fatalf("page 2 = %v, %q, %v", page, next, err)
	}
	all, _, err := accounts.List(c, account.ListParams{})
	if err != nil || len(all) != 3 {
		t.Fatalf("unlimited = %v, %v", all, err)
	}

	// ── api keys: hash lookup, unique hash, FK, nullable timestamps, lock ─
	secret := "xk_" + strings.Repeat("s", 43)
	if _, err := keys.FindByHash(c, apikey.HashSecret(secret)); !errors.Is(err, apikey.ErrNotFound) {
		t.Fatalf("missing hash: %v", err)
	}
	k, _ := apikey.New("k1", "a", "laptop", secret, time.Time{}, now)
	if err := keys.Add(c, k); err != nil {
		t.Fatal(err)
	}
	if err := keys.Add(c, k); !errors.Is(err, apikey.ErrDuplicate) {
		t.Fatalf("duplicate id: %v", err)
	}
	twin, _ := apikey.New("k2", "a", "twin", secret, time.Time{}, now)
	if err := keys.Add(c, twin); !errors.Is(err, apikey.ErrDuplicate) {
		t.Fatalf("duplicate hash: %v", err)
	}
	orphan, _ := apikey.New("k3", "nobody", "orphan", "xk_"+strings.Repeat("o", 43), time.Time{}, now)
	if err := keys.Add(c, orphan); err == nil || errors.Is(err, apikey.ErrDuplicate) {
		t.Fatalf("orphan key must fail the FK: %v", err)
	}
	byHash, err := keys.FindByHash(c, apikey.HashSecret(secret))
	if err != nil || byHash.ID() != "k1" || byHash.Version() != 1 || !byHash.ExpiresAt().IsZero() || !byHash.RevokedAt().IsZero() || byHash.Prefix() != secret[:apikey.PrefixLen] {
		t.Fatalf("by hash = %+v, %v", byHash, err)
	}
	exp := now.Add(time.Hour)
	k2, _ := apikey.New("k2", "a", "ci", "xk_"+strings.Repeat("t", 43), exp, now.Add(time.Second))
	if err := keys.Add(c, k2); err != nil {
		t.Fatal(err)
	}
	if l, _ := keys.FindByID(c, "k2"); !l.ExpiresAt().Equal(exp) {
		t.Fatalf("expires_at = %v, want %v", l.ExpiresAt(), exp)
	}
	stale, _ := keys.FindByID(c, "k1")
	if err := byHash.Revoke(now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := keys.Save(c, byHash); err != nil {
		t.Fatal(err)
	}
	if err := keys.Save(c, stale); !errors.Is(err, apikey.ErrStale) {
		t.Fatalf("stale save: %v", err)
	}
	revoked, _ := keys.FindByID(c, "k1")
	if revoked.Status() != apikey.StatusRevoked || !revoked.RevokedAt().Equal(now.Add(time.Minute)) || revoked.Version() != 2 {
		t.Fatalf("revoked = %+v", revoked)
	}
	list, err := keys.ListByAccount(c, "a")
	if err != nil || len(list) != 2 || list[0].ID() != "k2" || list[1].ID() != "k1" {
		t.Fatalf("list = %v, %v (want newest first)", list, err)
	}
	if list, _ := keys.ListByAccount(c, "b"); len(list) != 0 {
		t.Fatalf("list b = %v", list)
	}
	if err := db.WithContext(c).Exec("UPDATE identity.api_keys SET status = 'WEIRD' WHERE id = 'k1'").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := keys.FindByID(c, "k1"); !errors.Is(err, apikey.ErrInvalidStatus) {
		t.Fatalf("corrupt status: %v", err)
	}
	if _, err := keys.ListByAccount(c, "a"); !errors.Is(err, apikey.ErrInvalidStatus) {
		t.Fatalf("corrupt status in list: %v", err)
	}

	// ── last_used_at: conditional write, no version bump, Save does not touch it ─
	fresh, _ := keys.FindByID(c, "k2")
	if !fresh.LastUsedAt().IsZero() {
		t.Fatalf("k2 was never used, got %v", fresh.LastUsedAt())
	}
	window := time.Minute
	if ok, err := keys.TouchLastUsed(c, "k2", now, window); err != nil || !ok {
		t.Fatalf("first touch = %v, %v", ok, err)
	}
	if ok, err := keys.TouchLastUsed(c, "k2", now.Add(window-time.Second), window); err != nil || ok {
		t.Fatalf("touch inside window = %v, %v", ok, err)
	}
	if ok, err := keys.TouchLastUsed(c, "k2", now.Add(window), window); err != nil || !ok {
		t.Fatalf("touch after window = %v, %v", ok, err)
	}
	touched, _ := keys.FindByID(c, "k2")
	if !touched.LastUsedAt().Equal(now.Add(window)) || touched.Version() != fresh.Version() {
		t.Fatalf("touched = last_used %v version %d, want %v / %d", touched.LastUsedAt(), touched.Version(), now.Add(window), fresh.Version())
	}
	if err := keys.Save(c, fresh); err != nil {
		t.Fatalf("save of a pre-touch aggregate must not be stale: %v", err)
	}
	if after, _ := keys.FindByID(c, "k2"); !after.LastUsedAt().Equal(now.Add(window)) {
		t.Fatalf("save clobbered last_used_at: %v", after.LastUsedAt())
	}
	if ok, err := keys.TouchLastUsed(c, "nope", now, window); err != nil || ok {
		t.Fatalf("unknown id = %v, %v", ok, err)
	}

	// ── last_used_at under concurrency: the SQL condition, not a lock in Go, is what
	// lets one writer through per interval. Every round is 32 goroutines released
	// together at the same instant, against the real table. ───────────────────────
	racer, _ := apikey.New("k5", "a", "racer", "xk_"+strings.Repeat("r", 43), time.Time{}, now)
	if err := keys.Add(c, racer); err != nil {
		t.Fatal(err)
	}
	stampers := func(at time.Time) (stamped int) {
		t.Helper()
		const writers = 32
		var wg sync.WaitGroup
		start := make(chan struct{})
		results := make(chan bool, writers)
		for range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ok, err := keys.TouchLastUsed(c, "k5", at, time.Minute)
				if err != nil {
					t.Errorf("concurrent touch: %v", err)
				}
				results <- ok
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		for ok := range results {
			if ok {
				stamped++
			}
		}

		return stamped
	}
	t0 := now.Add(time.Hour)
	for _, round := range []struct {
		name string
		at   time.Time
		want int
	}{
		{"never used: exactly one writer wins", t0, 1},
		{"one microsecond before the interval ends: nobody", t0.Add(time.Minute - time.Microsecond), 0},
		{"exactly one interval later: exactly one writer wins", t0.Add(time.Minute), 1},
		{"inside the new interval: nobody", t0.Add(time.Minute + 30*time.Second), 0},
	} {
		if got := stampers(round.at); got != round.want {
			t.Fatalf("%s: %d writers stamped, want %d", round.name, got, round.want)
		}
	}
	if raced, _ := keys.FindByID(c, "k5"); !raced.LastUsedAt().Equal(t0.Add(time.Minute)) || raced.Version() != 1 {
		t.Fatalf("after the races: last_used %v version %d, want %v / 1", raced.LastUsedAt(), raced.Version(), t0.Add(time.Minute))
	}

	// Context errors come back as such, before any SQL.
	cancelled, cancel := context.WithCancel(c)
	cancel()
	if _, err := accounts.FindByID(cancelled, "a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	if _, err := keys.FindByHash(cancelled, apikey.HashSecret(secret)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}
