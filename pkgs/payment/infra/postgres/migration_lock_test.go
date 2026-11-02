package postgres_test

import (
	"context"
	"testing"
	"testing/fstest"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// docs/operations.md says what migration 0002 does to payment.payments while it runs. This pins the
// claim to the real runner (golang-migrate through platformpg.Up): a migration file shaped like 0002
// — ADD COLUMN, a plain CREATE INDEX — with a pause in the middle and a failing last statement.
//   - ADD COLUMN takes ACCESS EXCLUSIVE and the file is one transaction, so the lock is held to the
//     end of the file: a plain SELECT waits (not only writes), even while the file is only sleeping.
//   - The failure rolls the whole file back: no half-applied column or index is left.
func TestMigrationLock_AddColumnBlocksReadsUntilTheFileEnds(t *testing.T) {
	db := testDB(t)
	if err := db.Exec(insertSQL, "lk-1", "ord-lk-1", "cust", 10, "TWD", "PENDING", "NONE").Error; err != nil {
		t.Fatal(err)
	}

	// The probe's failure leaves its own version table dirty; start and end clean so reruns work.
	dropProbe := func() {
		if err := db.Exec(`DROP SCHEMA IF EXISTS lockprobe CASCADE`).Error; err != nil {
			t.Errorf("drop schema lockprobe: %v", err)
		}
	}
	dropProbe()
	t.Cleanup(dropProbe)

	probe := platformpg.Migration{Schema: "lockprobe", Files: fstest.MapFS{
		"0001_probe.up.sql": {Data: []byte(`
ALTER TABLE payment.payments ADD COLUMN lock_probe timestamptz;
SELECT pg_sleep(2);
CREATE INDEX lock_probe_idx ON payment.payments (lock_probe NULLS FIRST, expires_at) WHERE status = 'PENDING';
SELECT 1/0;
`)},
		"0001_probe.down.sql": {Data: []byte(`SELECT 1;`)},
	}}

	c := context.Background()
	url := pgtest.URL(t)
	reader := pgtest.Conn(t, url)

	done := make(chan error, 1)
	go func() { done <- platformpg.Up(c, config.Postgres{URL: url}, probe) }() // fire-and-forget: joined through done below

	// Wait until the migration's ADD COLUMN is holding the table, then read with a lock_timeout.
	if _, err := reader.Exec(c, `SET lock_timeout = '300ms'`); err != nil {
		t.Fatal(err)
	}
	blocked := false
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := reader.Exec(c, `SELECT 1 FROM payment.payments LIMIT 1`); err != nil {
			blocked = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("a SELECT on payment.payments never waited while the migration file was running; the doc must not say reads are unaffected")
	}

	if err := <-done; err == nil {
		t.Fatal("the probe migration ends in 1/0 and must fail")
	}

	// The failed file left nothing behind, and the table reads again at once.
	var n int
	if err := db.Raw(`SELECT count(*) FROM information_schema.columns WHERE table_schema = 'payment' AND table_name = 'payments' AND column_name = 'lock_probe'`).Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("the failed migration file left its ADD COLUMN behind: the file is not one transaction")
	}
	if _, err := reader.Exec(c, `SELECT 1 FROM payment.payments LIMIT 1`); err != nil {
		t.Fatalf("a SELECT after the migration ended: %v", err)
	}
}

// The two other claims in the same paragraph: a nullable ADD COLUMN without a DEFAULT does not
// rewrite the table (the relation keeps its file), and while ADD COLUMN is queued behind a
// transaction that already read the table, newer readers queue behind it too.
func TestMigrationLock_NoRewriteAndTheQueueBehindAddColumn(t *testing.T) {
	db := testDB(t)
	if err := db.Exec(insertSQL, "lk-2", "ord-lk-2", "cust", 10, "TWD", "PENDING", "NONE").Error; err != nil {
		t.Fatal(err)
	}
	c := context.Background()
	url := pgtest.URL(t)
	holder, migrator, newcomer := pgtest.Conn(t, url), pgtest.Conn(t, url), pgtest.Conn(t, url)

	var before, after string
	const filenode = `SELECT relfilenode::text FROM pg_class WHERE oid = 'payment.payments'::regclass`
	if err := db.Raw(filenode).Scan(&before).Error; err != nil {
		t.Fatal(err)
	}

	// holder has read the table and not finished: it keeps ACCESS SHARE.
	htx, err := holder.Begin(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = htx.Rollback(c) })
	if _, err := htx.Exec(c, `SELECT 1 FROM payment.payments LIMIT 1`); err != nil {
		t.Fatal(err)
	}

	// The migration's ADD COLUMN queues behind holder (it would wait forever; give it a timeout).
	if _, err := migrator.Exec(c, `SET lock_timeout = '1500ms'`); err != nil {
		t.Fatal(err)
	}
	mtx, err := migrator.Begin(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mtx.Rollback(c) })
	queued := make(chan error, 1)
	go func() { // fire-and-forget: joined through queued below
		_, err := mtx.Exec(c, `ALTER TABLE payment.payments ADD COLUMN lock_probe2 timestamptz`)
		queued <- err
	}()
	time.Sleep(300 * time.Millisecond) // let the ALTER reach the lock queue

	// A new reader, which would normally share ACCESS SHARE with holder, now waits behind the ALTER.
	if _, err := newcomer.Exec(c, `SET lock_timeout = '300ms'`); err != nil {
		t.Fatal(err)
	}
	if _, err := newcomer.Exec(c, `SELECT 1 FROM payment.payments LIMIT 1`); err == nil {
		t.Fatal("a new SELECT ran while ADD COLUMN was queued behind an open reader; the doc must not say it only matters once it has the lock")
	}
	if err := <-queued; err == nil {
		t.Fatal("ADD COLUMN got its lock while an earlier reader was still open")
	}
	_ = htx.Rollback(c)
	_ = mtx.Rollback(c)

	// Without the open reader the same ADD COLUMN keeps the table's file: no rewrite.
	tx, err := migrator.Begin(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(c, `ALTER TABLE payment.payments ADD COLUMN lock_probe3 timestamptz`); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(c, filenode).Scan(&after); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(c)
	if before != after {
		t.Fatalf("ADD COLUMN rewrote payment.payments (relfilenode %s -> %s)", before, after)
	}
}
