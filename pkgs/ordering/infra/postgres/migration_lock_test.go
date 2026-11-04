package postgres_test

import (
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// causeRefusedIn reports whether the CHECK refuses a history row with this cause (inserted in a
// transaction that is rolled back either way).
func causeRefusedIn(t *testing.T, db *gorm.DB, cause string) bool {
	t.Helper()
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer func() { _ = tx.Rollback().Error }()

	return tx.Exec(`INSERT INTO ordering.order_history (order_id, seq, event, from_status, to_status, occurred_at, actor_kind, actor_subject, cause)
		VALUES ('o-check-probe', 1, 'CANCELLED', 'CONFIRMED', 'CANCELLED', now(), 'SYSTEM', '', ?)`, cause).Error != nil
}

// 0011's ALTER TABLE needs a lock on ordering.order_history that every order write also needs (they
// append a history row in the order's transaction). A long transaction already holding the table must
// therefore make the migration give up quickly (lock_timeout) instead of queueing and stalling the
// write path behind it. Recovery from the dirty mark that leaves, and a rerun, then work.
//
// The holder is an ACCESS SHARE lock, what any plain SELECT on the table keeps for the length of its
// transaction. It is released by a timer as well, so a migration WITHOUT lock_timeout (the mutant) ends
// the test in a bounded time with a red assertion instead of hanging.
func TestMigration0011_GivesUpOnALockInsteadOfQueueing(t *testing.T) {
	c, db := historyDB(t)
	m := postgres.Migration()
	cfg := config.Postgres{URL: pgtest.URL(t)}

	// Leave the BC at its latest and clean whatever happens below (before the pool closes).
	t.Cleanup(func() {
		if err := db.WithContext(c).Exec(`UPDATE ordering.schema_migrations SET version = 10, dirty = false`).Error; err != nil {
			t.Errorf("cleanup: reset version: %v", err)
		}
		if err := platformpg.Up(c, cfg, m); err != nil {
			t.Errorf("cleanup: up: %v", err)
		}
	})

	if err := platformpg.Down(c, cfg, m); err != nil { // 11 → 10
		t.Fatal(err)
	}
	if v, dirty, err := platformpg.Version(c, cfg, m); err != nil || v != 10 || dirty {
		t.Fatalf("before: %d dirty=%v err=%v, want 10 clean", v, dirty, err)
	}

	holder := db.Begin()
	if holder.Error != nil {
		t.Fatal(holder.Error)
	}
	released := make(chan struct{})
	release := func() {
		select {
		case <-released:
		default:
			close(released)
			_ = holder.Rollback().Error
		}
	}
	defer release()
	if err := holder.Exec(`LOCK TABLE ordering.order_history IN ACCESS SHARE MODE`).Error; err != nil {
		t.Fatal(err)
	}
	timer := time.AfterFunc(12*time.Second, release)
	defer timer.Stop()

	start := time.Now()
	err := platformpg.Up(c, cfg, m)
	took := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "lock timeout") {
		t.Fatalf("Up with the table locked = %v after %s, want a lock timeout error", err, took)
	}
	if took > 8*time.Second {
		t.Errorf("Up gave up after %s, want it to fail within lock_timeout (3s) plus slack, not wait for the holder", took)
	}

	// The failed run changed nothing but the dirty mark: the old CHECK is still the one in force.
	if v, dirty, err := platformpg.Version(c, cfg, m); err != nil || v != 11 || !dirty {
		t.Errorf("after the timeout: %d dirty=%v err=%v, want 11 dirty (the version is marked before the file runs)", v, dirty, err)
	}
	if !causeRefusedIn(t, db, "STOCK_SHORTAGE") {
		t.Error("the timed-out migration left a widened CHECK behind")
	}

	// Until the mark is reset a rerun is refused, which is what stops an upgrade at the migrate hook.
	release()
	if err := platformpg.Up(c, cfg, m); err == nil || !strings.Contains(strings.ToLower(err.Error()), "dirty") {
		t.Errorf("Up on a dirty version = %v, want it refused as dirty", err)
	}

	// Recovery, as docs/operations.md gives it: release the long transaction, reset the mark to 10, rerun.
	release()
	if err := db.WithContext(c).Exec(`UPDATE ordering.schema_migrations SET version = 10, dirty = false`).Error; err != nil {
		t.Fatal(err)
	}
	if err := platformpg.Up(c, cfg, m); err != nil {
		t.Fatalf("rerun after the lock is gone: %v", err)
	}
	if v, dirty, err := platformpg.Version(c, cfg, m); err != nil || v != 11 || dirty {
		t.Errorf("after the rerun: %d dirty=%v err=%v, want 11 clean", v, dirty, err)
	}
	if causeRefusedIn(t, db, "STOCK_SHORTAGE") {
		t.Error("the rerun did not widen the CHECK")
	}
}

// NOT VALID skips the scan of existing rows, not the enforcement on new ones: the widened CHECK is
// present, not yet validated, and still refuses a cause outside the table while admitting the two new ones.
func TestMigration0011_NotValidStillEnforcesTheCheck(t *testing.T) {
	_, db := historyDB(t)

	var validated bool
	if err := db.Raw(`SELECT convalidated FROM pg_constraint WHERE conrelid = 'ordering.order_history'::regclass AND conname = 'order_history_cause_check'`).Scan(&validated).Error; err != nil {
		t.Fatal(err)
	}
	if validated {
		t.Error("order_history_cause_check is validated: 0011 was meant to add it NOT VALID (no table scan under the lock)")
	}
	if causeRefusedIn(t, db, "STOCK_SHORTAGE") || causeRefusedIn(t, db, "RESERVATION_EXPIRED") {
		t.Error("the widened CHECK refuses an inventory cause")
	}
	if !causeRefusedIn(t, db, "SOMETHING_ELSE") {
		t.Error("a NOT VALID CHECK must still refuse a new row outside the table")
	}
}
