package postgres_test

import (
	"context"
	"io/fs"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/postgres"
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

// docs/operations.md says migration 0003 (the mock provider's state tables) does not make payment.payments
// wait. Two pins: the real file only creates tables of its own (nothing that locks an existing table),
// and a file of that shape — new tables, a foreign key between them, a pause, a failure — lets reads and
// writes on payment.payments through while it runs and leaves nothing behind when it fails.
func TestMigrationLock_0003OnlyCreatesNewTables(t *testing.T) {
	files := postgres.Migration().Files
	b, err := fs.ReadFile(files, "0003_mock_provider.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	var sql strings.Builder
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		sql.WriteString(line + "\n")
	}
	stmts := 0
	for _, stmt := range strings.Split(sql.String(), ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		stmts++
		if !regexp.MustCompile(`(?is)^CREATE TABLE payment\.mock_(checkouts|refunds) \(`).MatchString(stmt) {
			t.Fatalf("0003 may only create the mock tables (anything else can lock a table the running service uses): %.80s", stmt)
		}
	}
	if stmts != 2 {
		t.Fatalf("0003 has %d statements, want the two CREATE TABLE", stmts)
	}
	// the only references are to its own table, never to payment.payments
	if strings.Contains(sql.String(), "payment.payments") {
		t.Fatal("0003 must not reference payment.payments")
	}
}

// 0004 (the mock's fault table) is held to the same shape as 0003: one new table of its own, no ALTER or
// index on an existing table, no reference to payment.payments.
func TestMigrationLock_0004OnlyCreatesANewTable(t *testing.T) {
	b, err := fs.ReadFile(postgres.Migration().Files, "0004_mock_faults.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	var sql strings.Builder
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		sql.WriteString(line + "\n")
	}
	stmts := 0
	for _, stmt := range strings.Split(sql.String(), ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		stmts++
		if !regexp.MustCompile(`(?is)^CREATE TABLE payment\.mock_faults \(`).MatchString(stmt) {
			t.Fatalf("0004 may only create payment.mock_faults (anything else can lock a table the running service uses): %.80s", stmt)
		}
	}
	if stmts != 1 {
		t.Fatalf("0004 has %d statements, want the one CREATE TABLE", stmts)
	}
	if strings.Contains(sql.String(), "payment.payments") {
		t.Fatal("0004 must not reference payment.payments")
	}
}

func TestMigrationLock_NewTablesDoNotBlockPayments(t *testing.T) {
	db := testDB(t)
	if err := db.Exec(insertSQL, "lk-3", "ord-lk-3", "cust", 10, "TWD", "PENDING", "NONE").Error; err != nil {
		t.Fatal(err)
	}
	dropProbe := func() {
		if err := db.Exec(`DROP SCHEMA IF EXISTS lockprobe3 CASCADE`).Error; err != nil {
			t.Errorf("drop schema lockprobe3: %v", err)
		}
	}
	dropProbe()
	t.Cleanup(dropProbe)

	probe := platformpg.Migration{Schema: "lockprobe3", Files: fstest.MapFS{
		"0001_probe.up.sql": {Data: []byte(`
CREATE TABLE lockprobe3.a (ref text PRIMARY KEY, seq bigserial NOT NULL UNIQUE);
CREATE TABLE lockprobe3.b (ref text NOT NULL REFERENCES lockprobe3.a (ref) ON DELETE CASCADE, k text NOT NULL, PRIMARY KEY (ref, k));
SELECT pg_sleep(2);
SELECT 1/0;
`)},
		"0001_probe.down.sql": {Data: []byte(`SELECT 1;`)},
	}}

	c := context.Background()
	url := pgtest.URL(t)
	user := pgtest.Conn(t, url)
	watcher := pgtest.Conn(t, url)

	done := make(chan error, 1)
	go func() { done <- platformpg.Up(c, config.Postgres{URL: url}, probe) }() // fire-and-forget: joined through done below

	// positive control: the migration is really inside its sleep (its statement is running) while we use the table
	deadline := time.Now().Add(5 * time.Second)
	running := false
	for time.Now().Before(deadline) && !running {
		var n int
		if err := watcher.QueryRow(c, `SELECT count(*) FROM pg_stat_activity WHERE state = 'active' AND query LIKE '%lockprobe3.a%' AND pid <> pg_backend_pid()`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		running = n > 0
		time.Sleep(20 * time.Millisecond)
	}
	if !running {
		t.Fatal("fixture: the probe migration was never seen running")
	}

	if _, err := user.Exec(c, `SET lock_timeout = '300ms'`); err != nil {
		t.Fatal(err)
	}
	if _, err := user.Exec(c, `SELECT 1 FROM payment.payments LIMIT 1`); err != nil {
		t.Fatalf("a SELECT on payment.payments while the migration ran: %v", err)
	}
	if _, err := user.Exec(c, `UPDATE payment.payments SET updated_at = now() WHERE id = 'lk-3'`); err != nil {
		t.Fatalf("an UPDATE on payment.payments while the migration ran: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("fixture: the migration ended before the reads finished: %v", err)
	default:
	}

	if err := <-done; err == nil {
		t.Fatal("the probe migration ends in 1/0 and must fail")
	}
	var n int
	if err := db.Raw(`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'lockprobe3' AND table_name IN ('a', 'b')`).Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("the failed file left %d tables behind: it is not one transaction", n)
	}
}
