package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// upTo keeps the migration files up to and including version n.
func upTo(t *testing.T, m platformpg.Migration, n int) platformpg.Migration {
	t.Helper()
	files := fstest.MapFS{}
	err := fs.WalkDir(m.Files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		var v int
		if _, err := fmt.Sscanf(d.Name(), "%d_", &v); err != nil {
			return err
		}
		if v <= n {
			b, err := fs.ReadFile(m.Files, path)
			if err != nil {
				return err
			}
			files[path] = &fstest.MapFile{Data: b}
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return platformpg.Migration{Schema: m.Schema, Files: files}
}

// 0009 gives inventory.stocks catalog's SKU rule (1–64 bytes) as a CHECK. Real Postgres:
// the CHECK refuses 65 bytes (and counts bytes, not characters); a table that already
// holds an over-long SKU fails the migration, leaves no constraint behind (golang-migrate
// marks the version dirty; docs/operations.md has the reset); once the row is dealt with
// and the version reset, up, down and up again all work.
func TestStocksSKUCheck_Postgres(t *testing.T) {
	var afterDirty func()
	if os.Getenv(faultEnv) == "1" {
		afterDirty = func() { injectFault(t) }
	}
	stocksSKUCheck(t, pgtest.URL(t), afterDirty)
}

// faultEnv makes TestStocksSKUCheck_Postgres fail on purpose once 0009 is dirty. Only
// the helper process below sets it, against the database the parent already owns.
const faultEnv = "SKU_CHECK_INJECT_FAULT"

const faultMsg = "injected failure while 0009 is dirty"

// injectFault runs in the helper process. It first proves the state worth cleaning
// exists (a dirty 9 and the over-long row, read over a connection of its own), so an
// empty database cannot pass for a restored one, then fails the real test with t.Fatal.
func injectFault(t *testing.T) {
	t.Helper()
	c := context.Background()
	cfg := config.Postgres{URL: pgtest.URL(t)}
	v, dirty, err := platformpg.Version(c, cfg, postgres.Migration())
	if err != nil || v != 9 || !dirty {
		t.Fatalf("before the fault: version = %d dirty=%v err=%v, want 9 dirty", v, dirty, err)
	}
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	db, closeDB, err := platformpg.NewDB(cfg, tel)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	var rows int
	if err := db.Raw(`SELECT count(*) FROM inventory.stocks WHERE octet_length(sku) = 65`).Scan(&rows).Error; err != nil || rows != 1 {
		t.Fatalf("before the fault: over-long rows = %d err=%v, want 1", rows, err)
	}
	t.Fatal(faultMsg)
}

// A failure between the dirty 0009 and the documented reset must not leave the shared
// database dirty for the next migration test. The real test is run in a helper process
// with the fault on (so t.Fatal and its cleanups run exactly as in a failing run);
// afterwards, over fresh connections, the schema has to be at the latest version, clean,
// with the over-long row gone and a validated CHECK, and the next fixture's Up works.
func TestStocksSKUCheck_FailureLeavesCleanDatabase(t *testing.T) {
	url := pgtest.URL(t)
	cfg := config.Postgres{URL: url}
	full := postgres.Migration()

	out, _, _, err := runHelperReaped(t, url, helperLimit(t))
	if errors.Is(err, errHelperTimeout) {
		t.Fatalf("the helper did not finish in time (this is not the injected fault): %v\n%s", err, out)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("the helper should have failed on the injected fault, err=%v\n%s", err, out)
	}
	if !strings.Contains(string(out), faultMsg) {
		t.Fatalf("the helper failed, but not on the injected fault:\n%s", out)
	}
	if strings.Contains(string(out), "restore:") {
		t.Fatalf("restore reported an error:\n%s", out)
	}

	c := context.Background()
	v, dirty, err := platformpg.Version(c, cfg, full)
	if err != nil || v != 9 || dirty {
		t.Fatalf("after the failed run: version = %d dirty=%v err=%v, want 9 clean\n%s", v, dirty, err, out)
	}
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
	var rows int
	if err := db.Raw(`SELECT count(*) FROM inventory.stocks WHERE octet_length(sku) > 64`).Scan(&rows).Error; err != nil || rows != 0 {
		t.Fatalf("over-long rows = %d, err=%v; want them gone", rows, err)
	}
	var checks int
	var validated bool
	if err := db.Raw(`SELECT count(*), COALESCE(bool_and(convalidated), false) FROM pg_constraint WHERE conrelid = 'inventory.stocks'::regclass AND contype = 'c' AND pg_get_constraintdef(oid) ILIKE '%octet_length%'`).Row().Scan(&checks, &validated); err != nil || checks == 0 || !validated {
		t.Fatalf("CHECK count = %d validated = %v, err=%v; want 0009's CHECK, validated", checks, validated, err)
	}
	if err := platformpg.Up(c, cfg, full); err != nil {
		t.Fatalf("the next fixture could not Up: %v", err)
	}
}

// With a session holding ACCESS EXCLUSIVE on inventory.stocks the helper blocks on its
// first DROP CONSTRAINT. The run has to end at the limit as a timeout (not as the
// injected fault), and neither the helper nor anything it forked may be left running.
func TestStocksSKUCheck_HelperIsBoundedAndReaped(t *testing.T) {
	url := pgtest.URL(t)
	cfg := config.Postgres{URL: url}
	full := postgres.Migration()
	c := context.Background()

	if err := platformpg.Up(c, cfg, full); err != nil {
		t.Fatal(err)
	}
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(telCleanup)
	db, closeDB, err := platformpg.NewDB(cfg, tel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeDB)

	tx := db.WithContext(c).Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	t.Cleanup(func() { tx.Rollback() })
	if err := tx.Exec("LOCK TABLE inventory.stocks IN ACCESS EXCLUSIVE MODE").Error; err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	out, pid, terminated, err := runHelperReaped(t, url, 5*time.Second)
	if !errors.Is(err, errHelperTimeout) {
		t.Fatalf("a helper stuck behind the lock should time out, err=%v\n%s", err, out)
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Fatalf("the helper was not stopped at its limit: took %v", took)
	}
	if strings.Contains(string(out), faultMsg) {
		t.Fatalf("the helper reached the injected fault despite the lock:\n%s", out)
	}
	if pid == 0 || groupAlive(pid) {
		t.Fatalf("the helper (pid %d) or a process of its group is still running", pid)
	}
	if terminated == 0 {
		t.Fatal("no server backend of the helper was found: it should have been waiting on the lock")
	}

	// Killing the client must not leave its DDL queued on the server: once the lock is
	// released, 0009's CHECK has to still be there, and after the restore the next
	// fixture finds a clean latest version.
	if err := tx.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		var n int
		if err := db.Raw(`SELECT count(*) FROM pg_constraint WHERE conrelid = 'inventory.stocks'::regclass AND contype = 'c' AND pg_get_constraintdef(oid) ILIKE '%octet_length%'`).Scan(&n).Error; err != nil || n == 0 {
			t.Fatalf("after the lock was released the CHECK count = %d, err=%v: the killed helper's DROP CONSTRAINT still ran on the server", n, err)
		}
	}
}

var errHelperTimeout = errors.New("helper timed out")

// helperLimit bounds the helper well inside what the test binary has left (so the parent
// is never killed first, orphaning the helper), and to a fixed cap with no deadline.
func helperLimit(t *testing.T) time.Duration {
	t.Helper()
	limit := 2 * time.Minute
	if d, ok := t.Deadline(); ok {
		if half := time.Until(d) / 2; half < limit {
			limit = half
		}
	}
	if limit <= 0 {
		t.Fatal("no time left to run the helper process")
	}

	return limit
}

// runFaultHelper runs the real TestStocksSKUCheck_Postgres with the fault on, in its own
// process group, with application_name app on its connections, and returns its output and pid. On limit it kills the whole group and
// waits for it, and reports errHelperTimeout instead of the helper's exit error.
func runFaultHelper(url, app string, limit time.Duration) (out []byte, pid int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStocksSKUCheck_Postgres$", "-test.count=1", "-test.v")
	// url comes from pgtest: the testcontainers database (or the one the caller opted into).
	cmd.Env = append(os.Environ(), faultEnv+"=1", pgtest.EnvURL+"="+url, "PGAPPNAME="+app)
	ownProcessGroup(cmd)
	cmd.WaitDelay = 10 * time.Second
	out, err = cmd.CombinedOutput()
	if cmd.Process != nil {
		pid = cmd.Process.Pid
	}
	if ctx.Err() != nil {
		return out, pid, fmt.Errorf("%w after %v: %w", errHelperTimeout, limit, err)
	}

	return out, pid, err
}

// runHelperReaped is the only way a test starts the helper. Before it starts, it
// registers the restore (which runs before the pool closes, errors reported), so no
// path out of the test, a timeout included, can skip it. After the helper ends, however
// it ended, its server backends are terminated and waited for: killing the client does
// not cancel a DDL the server is already waiting to run, and once the lock went away it
// would drop 0009's CHECK behind the restore's back. terminated is how many it found.
func runHelperReaped(t *testing.T, url string, limit time.Duration) (out []byte, pid, terminated int, err error) {
	t.Helper()
	cfg := config.Postgres{URL: url}
	tel, telCleanup, terr := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if terr != nil {
		t.Fatal(terr)
	}
	t.Cleanup(telCleanup)
	db, closeDB, derr := platformpg.NewDB(cfg, tel)
	if derr != nil {
		t.Fatal(derr)
	}
	t.Cleanup(closeDB)
	full := postgres.Migration()
	t.Cleanup(func() { restoreSKUState(t, cfg, db, full) })

	app := fmt.Sprintf("sku-helper-%d-%d", os.Getpid(), time.Now().UnixNano())
	out, pid, err = runFaultHelper(url, app, limit)
	terminated, rerr := reapBackends(db, app)
	if rerr != nil {
		t.Errorf("reap helper backends: %v", rerr)
	}

	return out, pid, terminated, err
}

// reapBackends terminates the server backends with application_name app and waits, up to
// 10s, until none is left. Not finding them gone is an error, never skipped.
func reapBackends(db *gorm.DB, app string) (terminated int, err error) {
	if err := db.Raw(`SELECT count(*) FROM (SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = ? AND pid <> pg_backend_pid()) t`, app).Scan(&terminated).Error; err != nil {
		return 0, fmt.Errorf("terminate: %w", err)
	}
	var left int
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if err := db.Raw(`SELECT count(*) FROM pg_stat_activity WHERE application_name = ? AND pid <> pg_backend_pid()`, app).Scan(&left).Error; err != nil {
			return terminated, fmt.Errorf("count: %w", err)
		}
		if left == 0 {
			return terminated, nil
		}
		if time.Now().After(deadline) {
			return terminated, fmt.Errorf("%d backends of %q still present after 10s", left, app)
		}
	}
}

// restoreSKUState leaves the shared database at the latest version, clean, whatever
// happened before: after a failure (or a killed helper) the version may be dirty, an
// over-long row may remain and the CHECK may be missing. Errors fail the test.
func restoreSKUState(t *testing.T, cfg config.Postgres, db *gorm.DB, full platformpg.Migration) {
	t.Helper()
	ctx := context.Background()
	v, dirty, err := platformpg.Version(ctx, cfg, full)
	if err != nil {
		t.Errorf("restore: read version: %v", err)

		return
	}
	if dirty {
		// A migration step is atomic, so the schema is at 9 exactly when 0009's CHECK exists.
		if err := db.WithContext(ctx).Exec(`UPDATE inventory.schema_migrations SET version = CASE WHEN EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'inventory.stocks'::regclass AND contype = 'c' AND pg_get_constraintdef(oid) ILIKE '%octet_length%') THEN 9 ELSE 8 END, dirty = false`).Error; err != nil {
			t.Errorf("restore: clear dirty version %d: %v", v, err)

			return
		}
	}
	if v > 0 {
		if err := db.WithContext(ctx).Exec("TRUNCATE inventory.stocks CASCADE").Error; err != nil {
			t.Errorf("restore: truncate stocks: %v", err)

			return
		}
	}
	if err := platformpg.Up(ctx, cfg, full); err != nil {
		t.Errorf("restore: up to latest: %v", err)
	}
}

func stocksSKUCheck(t *testing.T, url string, afterDirty func()) {
	c := context.Background()
	cfg := config.Postgres{URL: url}
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(telCleanup)
	db, cleanup, err := platformpg.NewDB(cfg, tel)
	if err != nil {
		t.Fatal(err)
	}
	// Registered first, so it runs last: the restore below still needs the pool.
	t.Cleanup(cleanup)

	full := postgres.Migration()
	before := upTo(t, full, 8)
	t.Cleanup(func() { restoreSKUState(t, cfg, db, full) })

	version := func() (uint, bool) {
		t.Helper()
		v, dirty, err := platformpg.Version(c, cfg, full)
		if err != nil {
			t.Fatal(err)
		}

		return v, dirty
	}
	hasCheck := func() bool {
		t.Helper()
		var n int
		if err := db.WithContext(c).Raw(`SELECT count(*) FROM pg_constraint WHERE conrelid = 'inventory.stocks'::regclass AND contype = 'c' AND pg_get_constraintdef(oid) ILIKE '%octet_length%'`).Scan(&n).Error; err != nil {
			t.Fatal(err)
		}

		return n > 0
	}
	insert := func(sku string) error {
		return db.WithContext(c).Exec(`INSERT INTO inventory.stocks (sku, on_hand, reserved, version, updated_at) VALUES (?, 1, 0, 1, now())`, sku).Error
	}

	// An over-long SKU from before the rule blocks the migration. Down steps back from the
	// recorded version, so start by bringing a fresh database to the latest one rather
	// than relying on an earlier test having run Up.
	if err := platformpg.Up(c, cfg, full); err != nil {
		t.Fatal(err)
	}
	if err := platformpg.Down(c, cfg, full); err != nil {
		t.Fatal(err)
	}
	if err := platformpg.Up(c, cfg, before); err != nil {
		t.Fatal(err)
	}
	if err := insert(strings.Repeat("a", 65)); err != nil {
		t.Fatal(err)
	}
	if err := platformpg.Up(c, cfg, full); err == nil {
		t.Fatal("migration 0009 succeeded over a 65-byte SKU")
	}
	if v, dirty := version(); v != 9 || !dirty {
		t.Fatalf("after the failed migration: version = %d dirty=%v, want 9 dirty", v, dirty)
	}
	if hasCheck() {
		t.Fatal("the failed migration left the CHECK behind")
	}
	if err := platformpg.Up(c, cfg, full); err == nil {
		t.Fatal("up on a dirty version succeeded")
	}
	if afterDirty != nil {
		afterDirty()
	}

	// Fix the data and reset the version (the documented recovery), then 0009 applies.
	if err := db.WithContext(c).Exec("TRUNCATE inventory.stocks CASCADE").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(c).Exec("UPDATE inventory.schema_migrations SET version = 8, dirty = false").Error; err != nil {
		t.Fatal(err)
	}
	if err := platformpg.Up(c, cfg, full); err != nil {
		t.Fatal(err)
	}
	if v, dirty := version(); v != 9 || dirty {
		t.Fatalf("version = %d dirty=%v, want 9 clean", v, dirty)
	}
	if !hasCheck() {
		t.Fatal("0009 did not add the CHECK")
	}
	var validated bool
	if err := db.WithContext(c).Raw(`SELECT bool_and(convalidated) FROM pg_constraint WHERE conrelid = 'inventory.stocks'::regclass AND contype = 'c' AND pg_get_constraintdef(oid) ILIKE '%octet_length%'`).Scan(&validated).Error; err != nil || !validated {
		t.Fatalf("CHECK validated = %v, %v; want a validated constraint", validated, err)
	}

	for name, sku := range map[string]string{"1 byte": "A", "64 bytes": strings.Repeat("a", 64), "21 chars of 3 bytes": strings.Repeat("中", 21)} {
		if err := insert(sku); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for name, sku := range map[string]string{"empty": "", "65 bytes": strings.Repeat("a", 65), "22 chars of 3 bytes = 66": strings.Repeat("中", 22)} {
		if err := insert(sku); err == nil {
			t.Fatalf("%s: the CHECK let it in", name)
		}
	}

	// Rerunnable: down removes the constraint, up puts it back.
	if err := db.WithContext(c).Exec("TRUNCATE inventory.stocks CASCADE").Error; err != nil {
		t.Fatal(err)
	}
	for _, step := range []func(context.Context, config.Postgres, platformpg.Migration) error{platformpg.Down, platformpg.Up} {
		if err := step(c, cfg, full); err != nil {
			t.Fatal(err)
		}
	}
	if v, dirty := version(); v != 9 || dirty || !hasCheck() {
		t.Fatalf("after down/up: version = %d dirty=%v check=%v", v, dirty, hasCheck())
	}
}
