package postgres_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/identity"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/apikey"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// openDB opens (and closes at cleanup) a handle on url.
func openDB(t *testing.T, url string) *gorm.DB {
	t.Helper()
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(telCleanup)
	db, cleanup, err := platformpg.NewDB(config.Postgres{URL: url}, tel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)

	return db
}

func randomName(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}

	return hex.EncodeToString(b)
}

// urlWith returns base pointed at database name (when not empty) and tagged
// with application_name app (when not empty).
func urlWith(t *testing.T, base, name, app string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse database url: %v", err)
	}
	if name != "" {
		u.Path = "/" + name
	}
	if app != "" {
		q := u.Query()
		q.Set("application_name", app)
		u.RawQuery = q.Encode()
	}

	return u.String()
}

// ownedDatabase creates a database only this test uses and returns its URL: the
// matrix empties identity.accounts and locks every row of it, which on the
// shared TEST_DATABASE_URL would reach into other tests' fixtures and make
// their admins part of this census. Scoped to this package on purpose.
//
// This needs CREATEDB on the server (the testcontainers default role has it; a
// supplied TEST_DATABASE_URL must too, see README). Without it the test fails
// with that explanation: sharing the supplied database instead would bring the
// interference back, and skipping would hide the regression.
func ownedDatabase(t *testing.T) string {
	t.Helper()
	base := pgtest.URL(t)
	name := "lockexp_" + randomName(t)
	admin := openDB(t, base)
	if err := admin.Exec("CREATE DATABASE " + name).Error; err != nil {
		t.Fatalf("create database %s: %v (the identity expiry matrix needs a role with CREATEDB; see README, TEST_DATABASE_URL)", name, err)
	}
	t.Cleanup(func() {
		if err := admin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)").Error; err != nil {
			t.Errorf("drop database %s: %v", name, err)
		}
	})
	owned := urlWith(t, base, name, "")
	if err := platformpg.Up(context.Background(), config.Postgres{URL: owned}, postgres.Migration()); err != nil {
		t.Fatal(err)
	}

	return owned
}

// lockHolder is a second transaction sitting on the admin rows, the "someone
// else is mid-write" the guard has to queue behind.
type lockHolder struct {
	tx  *gorm.DB
	pid int
}

func holdAdminRows(t *testing.T, db *gorm.DB) *lockHolder {
	t.Helper()
	tx := db.Begin()
	if err := tx.Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback() }) // no-op once committed
	var pid int
	if err := tx.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec("SELECT 1 FROM identity.accounts FOR UPDATE").Error; err != nil {
		t.Fatal(err)
	}

	return &lockHolder{tx: tx, pid: pid}
}

// queuedBehind reports whether the backend of the writer tagged writerApp is
// blocked by holder right now (pg_stat_activity + pg_blocking_pids). Pinning the
// writer, not just the holder, keeps an unrelated waiter from passing for it.
func (h *lockHolder) queuedBehind(t *testing.T, ctx context.Context, db *gorm.DB, writerApp string) bool {
	t.Helper()
	var blocked int
	if err := db.WithContext(ctx).Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name = ? AND ? = ANY(pg_blocking_pids(pid))", writerApp, h.pid).Scan(&blocked).Error; err != nil {
		t.Fatalf("poll pg_blocking_pids: %v", err)
	}

	return blocked > 0
}

// waitQueuedBehind returns once queuedBehind holds. The wait is on that lock
// state, with a bound, not on elapsed time: there is no sleep to be too short on
// a slow runner.
func (h *lockHolder) waitQueuedBehind(t *testing.T, db *gorm.DB, writerApp string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !h.queuedBehind(t, ctx, db, writerApp) {
		select {
		case <-ctx.Done():
			t.Fatalf("writer %s never queued behind the lock holder (pid %d)", writerApp, h.pid)
		case <-tick.C:
		}
	}
}

// release lets the queued writer through.
func (h *lockHolder) release(t *testing.T) {
	t.Helper()
	if err := h.tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
}

// expiryRun is one row of the matrix: the production use case (built by the
// identity provider, the way apps/rpc gets it) is called while the admin rows
// are locked, the key's expiry passes during the wait, and the lock is released.
type expiryRun struct {
	db        *gorm.DB // holder side; the writer has its own handle
	writerApp string
	clock     *stepClock
}

func (r expiryRun) do(t *testing.T, call func() error) error {
	t.Helper()
	h := holdAdminRows(t, r.db)
	done := make(chan error, 1)
	go func() { done <- call() }()
	h.waitQueuedBehind(t, r.db, r.writerApp)
	// The writer is parked on the lock and has not looked at the clock after it.
	r.clock.advance(2 * time.Hour)
	h.release(t)

	select {
	case err := <-done:
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("the writer never returned after the lock was released")
		return nil
	}
}

// What "usable" means for a backup key is decided once the lock is held, not
// when the writer started queueing for it. The matrix: each way the last admin
// can be removed, with the backup standing only until the clock moves.
func TestLastAdminGuard_ExpiryDuringLockWaitProviderPostgres(t *testing.T) {
	owned := ownedDatabase(t)
	db := openDB(t, owned)
	f := newCensusFixture(t, db)
	rec, err := usecasex.NewRecorder(noop.NewMeterProvider())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Auth{AdminRole: censusRole}
	c := context.Background()

	// setup gives each case its own clock and wiring, built like the provider does.
	type env struct {
		run     expiryRun
		disable func(account.ID) error
		assign  func(account.ID) error
		revoke  func(apikey.ID) error
	}
	setup := func(t *testing.T) env {
		t.Helper()
		t.Cleanup(func() { db.Exec("TRUNCATE identity.accounts CASCADE") })
		clock := &stepClock{t: f.now}
		// The writer under test gets a handle of its own, tagged so the barrier can
		// tell its backend from anything else on the server.
		app := fmt.Sprintf("lockexp-writer-%s", randomName(t))
		wdb := openDB(t, urlWith(t, owned, "", app))
		accounts, keys, tx := postgres.NewAccountRepository(wdb), postgres.NewAPIKeyRepository(wdb), postgres.NewTransactor(wdb)
		census := identity.ProvideAdminCensus(wdb, accounts, keys, clock)
		disable := identity.ProvideDisableAccount(accounts, census, cfg, clock, tx, rec)
		assign := identity.ProvideAssignRoles(accounts, census, cfg, clock, tx, rec)
		revoke := identity.ProvideRevokeAPIKey(keys, census, cfg, clock, tx, rec)

		return env{
			run: expiryRun{db: db, writerApp: app, clock: clock},
			disable: func(id account.ID) error {
				_, err := disable.Execute(c, dto.DisableAccountInput{ID: id.String()})
				return err
			},
			assign: func(id account.ID) error {
				_, err := assign.Execute(c, dto.AssignRolesInput{ID: id.String(), Roles: []string{"viewer"}})
				return err
			},
			revoke: func(id apikey.ID) error {
				_, err := revoke.Execute(c, dto.RevokeAPIKeyInput{ID: id.String()})
				return err
			},
		}
	}
	soon := f.now.Add(time.Hour)

	// refused asserts ErrLastAdmin and that the rejected write left everything as it was.
	refused := func(t *testing.T, err error, accounts []account.ID, keys []apikey.ID) {
		t.Helper()
		if !errors.Is(err, account.ErrLastAdmin) {
			t.Fatalf("err = %v, want ErrLastAdmin", err)
		}
		for _, id := range accounts {
			a, err := f.accounts.FindByID(c, id)
			if err != nil {
				t.Fatal(err)
			}
			if !a.IsActive() || !a.HasRole(censusRole) {
				t.Fatalf("account %s = %s %v after a refused write, want ACTIVE with %s", id, a.Status(), a.Roles(), censusRole)
			}
		}
		for _, id := range keys {
			k, err := f.keys.FindByID(c, id)
			if err != nil {
				t.Fatal(err)
			}
			if k.Status() != apikey.StatusActive {
				t.Fatalf("key %s = %s after a refused write, want active", id, k.Status())
			}
		}
	}

	t.Run("disable: the backup admin's only key expires", func(t *testing.T) {
		e := setup(t)
		a, akeys := f.account("m-dis-a", []string{censusRole}, time.Time{})
		b, bkeys := f.account("m-dis-b", []string{censusRole}, soon)
		refused(t, e.run.do(t, func() error { return e.disable(a) }), []account.ID{a, b}, append(akeys, bkeys...))
	})

	t.Run("assign roles: the backup admin's only key expires", func(t *testing.T) {
		e := setup(t)
		a, akeys := f.account("m-as-a", []string{censusRole}, time.Time{})
		b, bkeys := f.account("m-as-b", []string{censusRole}, soon)
		refused(t, e.run.do(t, func() error { return e.assign(a) }), []account.ID{a, b}, append(akeys, bkeys...))
	})

	t.Run("revoke: the backup admin's only key expires", func(t *testing.T) {
		e := setup(t)
		a, akeys := f.account("m-rv-a", []string{censusRole}, time.Time{})
		b, bkeys := f.account("m-rv-b", []string{censusRole}, soon)
		refused(t, e.run.do(t, func() error { return e.revoke(akeys[0]) }), []account.ID{a, b}, append(akeys, bkeys...))
	})

	t.Run("revoke: the other key of the same admin expires", func(t *testing.T) {
		e := setup(t)
		a, keys := f.account("m-same", []string{censusRole}, time.Time{}, soon)
		refused(t, e.run.do(t, func() error { return e.revoke(keys[0]) }), []account.ID{a}, keys)
	})

	// The same wait with nothing expiring must go through: otherwise every
	// case above would pass for the wrong reason (the lock itself refusing, or
	// the census always seeing a lone admin).
	t.Run("control: a backup that outlives the wait lets the write through", func(t *testing.T) {
		e := setup(t)
		a, _ := f.account("m-ctl-a", []string{censusRole}, time.Time{})
		f.account("m-ctl-b", []string{censusRole}, time.Time{})
		if err := e.run.do(t, func() error { return e.disable(a) }); err != nil {
			t.Fatalf("err = %v, want the disable to succeed", err)
		}
		got, err := f.accounts.FindByID(c, a)
		if err != nil {
			t.Fatal(err)
		}
		if got.IsActive() {
			t.Fatal("account still ACTIVE after a permitted disable")
		}
	})
}

// The barrier identifies the writer under test: another application queued on
// the same holder is real lock contention but not the provider call.
func TestLockExpiryBarrier_PinsTheWriterPostgres(t *testing.T) {
	owned := ownedDatabase(t)
	db := openDB(t, owned)
	f := newCensusFixture(t, db)
	f.account("pin-a", []string{censusRole}, time.Time{})
	h := holdAdminRows(t, db)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const other = "lockexp-unrelated"
	otherDB := openDB(t, urlWith(t, owned, "", other))
	result := make(chan error, 1)
	go func() { result <- otherDB.WithContext(ctx).Exec("SELECT 1 FROM identity.accounts FOR UPDATE").Error }()
	h.waitQueuedBehind(t, db, other) // precondition: it is queued behind the holder

	if h.queuedBehind(t, ctx, db, "lockexp-writer-not-started") {
		t.Fatal("the barrier accepted an application that never ran as the writer")
	}
	h.release(t) // let the unrelated waiter finish before its handle is closed
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("the unrelated waiter failed after the lock was released: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("the unrelated waiter never finished after the lock was released")
	}
}

// The matrix empties and locks identity.accounts in its own database; an account
// that already lives in the shared (supplied) database must not notice. The
// shared database is only migrated here, never emptied: the bystander is
// uniquely named and is the only row this test removes.
func TestLockExpiryMatrix_LeavesTheSharedDatabaseAlonePostgres(t *testing.T) {
	base := pgtest.URL(t)
	shared := openDB(t, base)
	if err := platformpg.Up(context.Background(), config.Postgres{URL: base}, postgres.Migration()); err != nil {
		t.Fatal(err)
	}
	sf := newCensusFixture(t, shared)
	// Keyless: the fixture's key secrets repeat from run to run.
	bystander, _ := sf.account("bystander-"+randomName(t), []string{"viewer"})
	t.Cleanup(func() { shared.Exec("DELETE FROM identity.accounts WHERE id = ?", bystander.String()) })

	t.Run("matrix", TestLastAdminGuard_ExpiryDuringLockWaitProviderPostgres)

	if _, err := sf.accounts.FindByID(context.Background(), bystander); err != nil {
		t.Fatalf("an account of the shared database is gone after the matrix: %v", err)
	}
}
