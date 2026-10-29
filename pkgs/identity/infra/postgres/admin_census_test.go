package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/apikey"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

const censusRole = "admin"

// censusDB is a migrated, emptied identity schema.
func censusDB(t *testing.T) *gorm.DB {
	t.Helper()
	c := context.Background()
	cfg := config.Postgres{URL: pgtest.URL(t)}
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(telCleanup)
	db, cleanup, err := platformpg.NewDB(cfg, tel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	if err := platformpg.Up(c, cfg, postgres.Migration()); err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(c).Exec("TRUNCATE identity.accounts CASCADE").Error; err != nil {
		t.Fatal(err)
	}

	return db
}

type censusFixture struct {
	t        *testing.T
	accounts *postgres.AccountRepository
	keys     *postgres.APIKeyRepository
	now      time.Time
	n        int
}

func newCensusFixture(t *testing.T, db *gorm.DB) *censusFixture {
	return &censusFixture{t: t, accounts: postgres.NewAccountRepository(db), keys: postgres.NewAPIKeyRepository(db), now: time.Now().UTC().Truncate(time.Microsecond)}
}

// account adds an ACTIVE account with the roles and one key per expiry given (zero: never).
func (f *censusFixture) account(name string, roles []string, expiries ...time.Time) (account.ID, []apikey.ID) {
	f.t.Helper()
	c := context.Background()
	id := account.ID("acc-" + name)
	a, err := account.New(id, name, roles, f.now)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.accounts.Add(c, a); err != nil {
		f.t.Fatal(err)
	}
	var kids []apikey.ID
	for _, exp := range expiries {
		f.n++
		kid := apikey.ID(fmt.Sprintf("key-%s-%d", name, f.n))
		k, err := apikey.New(kid, id.String(), "k", fmt.Sprintf("xk_%040d", f.n), exp, f.now)
		if err != nil {
			f.t.Fatal(err)
		}
		if err := f.keys.Add(c, k); err != nil {
			f.t.Fatal(err)
		}
		kids = append(kids, kid)
	}

	return id, kids
}

type standings map[account.ID]int

func toStandings(admins []account.AdminStanding) standings {
	out := standings{}
	for _, s := range admins {
		out[s.ID()] = s.UsableKeys()
	}

	return out
}

func TestAdminCensus_Postgres(t *testing.T) {
	db := censusDB(t)
	f := newCensusFixture(t, db)
	c := context.Background()
	at := &stepClock{t: f.now}
	census := postgres.NewAdminCensus(db, at)
	soon := f.now.Add(time.Hour)

	f.account("two", []string{censusRole, "ops"}, time.Time{}, soon)
	f.account("keyless", []string{censusRole})
	f.account("viewer", []string{"viewer"}, time.Time{})
	revoked, keys := f.account("revoked", []string{censusRole}, time.Time{})
	f.account("expiring", []string{censusRole}, soon)
	gone, _ := f.account("disabled", []string{censusRole}, time.Time{})

	k, err := f.keys.FindByID(c, keys[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Revoke(f.now); err != nil {
		t.Fatal(err)
	}
	if err := f.keys.Save(c, k); err != nil {
		t.Fatal(err)
	}
	a, err := f.accounts.FindByID(c, gone)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Disable(f.now); err != nil {
		t.Fatal(err)
	}
	if err := f.accounts.Save(c, a); err != nil {
		t.Fatal(err)
	}
	_ = revoked

	admins, judged, err := census.LockAdmins(c, censusRole)
	if err != nil {
		t.Fatal(err)
	}
	if !judged.Equal(f.now) {
		t.Fatalf("judged at %v, want the clock's %v", judged, f.now)
	}
	if got := toStandings(admins); len(got) != 2 || got["acc-two"] != 2 || got["acc-expiring"] != 1 {
		t.Fatalf("census at now = %v, want acc-two:2 acc-expiring:1", got)
	}
	// An hour on, the expiring key no longer counts, and neither does its account.
	at.advance(time.Hour + time.Second)
	admins, _, err = census.LockAdmins(c, censusRole)
	if err != nil {
		t.Fatal(err)
	}
	if got := toStandings(admins); len(got) != 1 || got["acc-two"] != 1 {
		t.Fatalf("census later = %v, want acc-two:1", got)
	}
	if admins, _, err = census.LockAdmins(c, "nobody"); err != nil || len(admins) != 0 {
		t.Fatalf("census of an unheld role = %v, %v", admins, err)
	}
	// A role name is data, not SQL or JSON syntax.
	if admins, _, err = census.LockAdmins(c, `ad"min'] OR true --`); err != nil || len(admins) != 0 {
		t.Fatalf("census of a hostile role = %v, %v", admins, err)
	}
}

// rendezvous: see the memory test of the same name in application/usecase.
type rendezvous struct {
	port.AdminCensus
	arrived atomic.Int32
	both    chan struct{}
}

func (r *rendezvous) LockAdmins(c context.Context, role string) ([]account.AdminStanding, time.Time, error) {
	admins, now, err := r.AdminCensus.LockAdmins(c, role)
	if r.arrived.Add(1) == 2 {
		close(r.both)
	}
	select {
	case <-r.both:
	case <-time.After(500 * time.Millisecond):
	}

	return admins, now, err
}

type fixedClock struct{ t time.Time }

func (i fixedClock) Now() time.Time { return i.t }

func raceExactlyOneRefused(t *testing.T, calls ...func() error) {
	t.Helper()
	errs := make([]error, len(calls))
	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = call()
		}()
	}
	wg.Wait()
	ok, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, account.ErrLastAdmin):
			refused++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || refused != 1 {
		t.Fatalf("results = %v; want exactly one success and one ErrLastAdmin", errs)
	}
}

// The census locks the admin rows for the transaction: writers that each
// decide on a census taken before the other committed would both pass.
func TestLastAdminGuard_ConcurrentPostgres(t *testing.T) {
	db := censusDB(t)
	f := newCensusFixture(t, db)
	tx := postgres.NewTransactor(db)
	clock := fixedClock{t: f.now}
	newCensus := func() *rendezvous {
		return &rendezvous{AdminCensus: postgres.NewAdminCensus(db, clock), both: make(chan struct{})}
	}
	c := context.Background()

	t.Run("two admins disable each other", func(t *testing.T) {
		a, _ := f.account("mutual-a", []string{censusRole}, time.Time{})
		b, _ := f.account("mutual-b", []string{censusRole}, time.Time{})
		t.Cleanup(func() { db.Exec("TRUNCATE identity.accounts CASCADE") })
		disable := usecasex.Chain[dto.DisableAccountInput, *dto.AccountOutput](
			usecase.NewDisableAccount(f.accounts, newCensus(), censusRole, clock),
			usecasex.Transactional[dto.DisableAccountInput, *dto.AccountOutput](tx),
		)
		raceExactlyOneRefused(t,
			func() error { _, err := disable.Execute(c, dto.DisableAccountInput{ID: a.String()}); return err },
			func() error { _, err := disable.Execute(c, dto.DisableAccountInput{ID: b.String()}); return err },
		)
		admins, _, err := postgres.NewAdminCensus(db, clock).LockAdmins(c, censusRole)
		if err != nil || len(admins) != 1 {
			t.Fatalf("admins left = %v, %v; want exactly one", admins, err)
		}
	})

	t.Run("two keys of the only admin revoked at once", func(t *testing.T) {
		_, kids := f.account("keys", []string{censusRole}, time.Time{}, time.Time{})
		t.Cleanup(func() { db.Exec("TRUNCATE identity.accounts CASCADE") })
		revoke := usecasex.Chain[dto.RevokeAPIKeyInput, *dto.APIKeyOutput](
			usecase.NewRevokeAPIKey(f.keys, newCensus(), censusRole, clock),
			usecasex.Transactional[dto.RevokeAPIKeyInput, *dto.APIKeyOutput](tx),
		)
		raceExactlyOneRefused(t,
			func() error { _, err := revoke.Execute(c, dto.RevokeAPIKeyInput{ID: kids[0].String()}); return err },
			func() error { _, err := revoke.Execute(c, dto.RevokeAPIKeyInput{ID: kids[1].String()}); return err },
		)
		admins, _, err := postgres.NewAdminCensus(db, clock).LockAdmins(c, censusRole)
		if err != nil || len(admins) != 1 || admins[0].UsableKeys() != 1 {
			t.Fatalf("admins left = %v, %v; want one with one key", admins, err)
		}
	})

	t.Run("one admin loses its role while the other is disabled", func(t *testing.T) {
		a, _ := f.account("mix-a", []string{censusRole}, time.Time{})
		b, _ := f.account("mix-b", []string{censusRole}, time.Time{})
		t.Cleanup(func() { db.Exec("TRUNCATE identity.accounts CASCADE") })
		census := newCensus()
		disable := usecasex.Chain[dto.DisableAccountInput, *dto.AccountOutput](
			usecase.NewDisableAccount(f.accounts, census, censusRole, clock),
			usecasex.Transactional[dto.DisableAccountInput, *dto.AccountOutput](tx),
		)
		assign := usecasex.Chain[dto.AssignRolesInput, *dto.AccountOutput](
			usecase.NewAssignRoles(f.accounts, census, censusRole, clock),
			usecasex.Transactional[dto.AssignRolesInput, *dto.AccountOutput](tx),
		)
		raceExactlyOneRefused(t,
			func() error { _, err := disable.Execute(c, dto.DisableAccountInput{ID: a.String()}); return err },
			func() error {
				_, err := assign.Execute(c, dto.AssignRolesInput{ID: b.String(), Roles: []string{"viewer"}})
				return err
			},
		)
	})
}

// stepClock is a clock the test moves.
type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (i *stepClock) Now() time.Time {
	i.mu.Lock()
	defer i.mu.Unlock()

	return i.t
}

func (i *stepClock) advance(d time.Duration) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.t = i.t.Add(d)
}
