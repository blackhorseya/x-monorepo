package postgres_test

import (
	"context"
	"errors"
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
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

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

// waitQueuedBehind returns once Postgres itself reports a backend blocked by
// holder (pg_blocking_pids). The wait is on that lock state, with a bound, not
// on elapsed time: there is no sleep to be too short on a slow runner.
func (h *lockHolder) waitQueuedBehind(t *testing.T, db *gorm.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		var blocked int
		if err := db.WithContext(ctx).Raw("SELECT count(*) FROM pg_stat_activity WHERE ? = ANY(pg_blocking_pids(pid))", h.pid).Scan(&blocked).Error; err != nil {
			t.Fatalf("poll pg_blocking_pids: %v", err)
		}
		if blocked > 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("no backend ever queued behind the lock holder (pid %d)", h.pid)
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
	db    *gorm.DB
	clock *stepClock
}

func (r expiryRun) do(t *testing.T, call func() error) error {
	t.Helper()
	h := holdAdminRows(t, r.db)
	done := make(chan error, 1)
	go func() { done <- call() }()
	h.waitQueuedBehind(t, r.db)
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
	db := censusDB(t)
	f := newCensusFixture(t, db)
	tx := postgres.NewTransactor(db)
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
		census := identity.ProvideAdminCensus(db, f.accounts, f.keys, clock)
		disable := identity.ProvideDisableAccount(f.accounts, census, cfg, clock, tx, rec)
		assign := identity.ProvideAssignRoles(f.accounts, census, cfg, clock, tx, rec)
		revoke := identity.ProvideRevokeAPIKey(f.keys, census, cfg, clock, tx, rec)

		return env{
			run:     expiryRun{db: db, clock: clock},
			disable: func(id account.ID) error { _, err := disable.Execute(c, dto.DisableAccountInput{ID: id.String()}); return err },
			assign: func(id account.ID) error {
				_, err := assign.Execute(c, dto.AssignRolesInput{ID: id.String(), Roles: []string{"viewer"}})
				return err
			},
			revoke: func(id apikey.ID) error { _, err := revoke.Execute(c, dto.RevokeAPIKeyInput{ID: id.String()}); return err },
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
