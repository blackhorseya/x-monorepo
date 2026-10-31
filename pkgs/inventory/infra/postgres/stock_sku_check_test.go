package postgres_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"regexp"
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

const (
	// faultEnv makes TestStocksSKUCheck_Postgres fail on purpose once 0009 is dirty; only the
	// isolation test below sets it, on a child process.
	faultEnv = "SKU_CHECK_INJECT_FAULT"
	faultMsg = "injected failure while 0009 is dirty"
	// dbNameEnv names the database the child creates, so the parent can drop it if the child
	// is killed before its cleanup runs.
	dbNameEnv = "SKU_CHECK_DB_NAME"
)

var dbNamePattern = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

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

// dbURL points rawURL at database name and changes nothing else (credentials, host and
// every query parameter, application_name included, stay).
func dbURL(t *testing.T, rawURL, name string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" {
		t.Fatalf("parse database url: %v", err)
	}
	u.Path = "/" + name

	return u.String()
}

func randomName(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}

	return hex.EncodeToString(b)
}

func openDB(t *testing.T, rawURL string) *gorm.DB {
	t.Helper()
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(telCleanup)
	db, closeDB, err := platformpg.NewDB(config.Postgres{URL: rawURL}, tel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeDB)

	return db
}

// dropDatabase drops name for good: WITH (FORCE) ends every session still on it, a lock
// held or a statement waiting included, so there is nothing to restore afterwards.
func dropDatabase(db *gorm.DB, name string) error {
	if !dbNamePattern.MatchString(name) {
		return fmt.Errorf("refusing to drop database %q", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	return db.WithContext(ctx).Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)").Error
}

// ownedDatabase creates a database only this test uses, drops it when the test ends, and
// returns its URL. The 0009 test steps migrations down and leaves the version dirty on
// purpose, which on the shared TEST_DATABASE_URL would reach into other tests' fixtures,
// and a failure halfway would hand them that state.
//
// This needs CREATEDB (the testcontainers default role has it; a supplied
// TEST_DATABASE_URL must too, as for pkgs/identity's expiry matrix). Without it the test
// fails with that explanation: sharing the supplied database instead would bring the
// interference back, and skipping would hide it.
func ownedDatabase(t *testing.T) string {
	t.Helper()
	base := pgtest.URL(t)
	name := "skucheck_" + randomName(t)
	if v := os.Getenv(dbNameEnv); v != "" {
		name = v
	}
	if !dbNamePattern.MatchString(name) {
		t.Fatalf("database name %q is not usable", name)
	}
	admin := openDB(t, base)
	if err := admin.Exec("CREATE DATABASE " + name).Error; err != nil {
		t.Fatalf("create database %s: %v (the 0009 test needs a role with CREATEDB; see TEST_DATABASE_URL in pgtest)", name, err)
	}
	t.Cleanup(func() {
		if err := dropDatabase(admin, name); err != nil {
			t.Errorf("drop database %s: %v", name, err)
		}
	})

	return dbURL(t, base, name)
}

// 0009 gives inventory.stocks catalog's SKU rule (1–64 bytes) as a CHECK. Real Postgres,
// in a database of its own: the CHECK refuses 65 bytes (and counts bytes, not
// characters); a table that already holds an over-long SKU fails the migration, leaves no
// constraint behind (golang-migrate marks the version dirty; docs/operations.md has the
// reset); once the row is dealt with and the version reset, up, down and up again all
// work. It starts from an empty database, so it does not depend on another test having
// run Up, and nothing it leaves behind reaches another test.
func TestStocksSKUCheck_Postgres(t *testing.T) {
	c := context.Background()
	cfg := config.Postgres{URL: ownedDatabase(t)}
	db := openDB(t, cfg.URL)

	full := postgres.Migration()
	before := upTo(t, full, 8)

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

	// An over-long SKU from before the rule blocks the migration.
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
	if os.Getenv(faultEnv) == "1" {
		var rows int
		if err := db.WithContext(c).Raw(`SELECT count(*) FROM inventory.stocks WHERE octet_length(sku) = 65`).Scan(&rows).Error; err != nil || rows != 1 {
			t.Fatalf("before the fault: over-long rows = %d err=%v, want 1", rows, err)
		}
		t.Fatal(faultMsg) // dirty 9, a bad row, no CHECK: all of it stays in this database
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

// The URL the test derives keeps every parameter of TEST_DATABASE_URL.
func TestDBURL_KeepsEverythingButTheDatabase(t *testing.T) {
	got := dbURL(t, "postgres://u:p%40ss@h:5433/shared?sslmode=disable&application_name=probe&connect_timeout=5", "skucheck_x")
	want := "postgres://u:p%40ss@h:5433/skucheck_x?sslmode=disable&application_name=probe&connect_timeout=5"
	if got != want {
		t.Fatalf("dbURL = %q, want %q", got, want)
	}
}

// snapshot is what another test would see of the shared database.
type snapshot struct {
	Version uint
	Dirty   bool
	Checks  int
	Rows    int
}

func takeSnapshot(t *testing.T, cfg config.Postgres, db *gorm.DB) snapshot {
	t.Helper()
	v, dirty, err := platformpg.Version(context.Background(), cfg, postgres.Migration())
	if err != nil {
		t.Fatal(err)
	}
	s := snapshot{Version: v, Dirty: dirty}
	if err := db.Raw(`SELECT count(*) FROM pg_constraint WHERE conrelid = 'inventory.stocks'::regclass AND contype = 'c' AND pg_get_constraintdef(oid) ILIKE '%octet_length%'`).Scan(&s.Checks).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Raw(`SELECT count(*) FROM inventory.stocks`).Scan(&s.Rows).Error; err != nil {
		t.Fatal(err)
	}

	return s
}

// runSelf runs this test binary for the tests matching run, on the database at rawURL, in
// its own process group, killed (group and all) and waited for when limit passes.
func runSelf(rawURL, run string, env []string, limit time.Duration, args ...string) (out []byte, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=" + run, "-test.count=1", "-test.v"}, args...)...)
	cmd.Env = append(append(os.Environ(), pgtest.EnvURL+"="+rawURL), env...)
	ownProcessGroup(cmd)
	cmd.WaitDelay = 10 * time.Second
	out, err = cmd.CombinedOutput()
	if ctx.Err() != nil {
		return out, fmt.Errorf("child still running after %v: %w", limit, err)
	}

	return out, err
}

// The 0009 test has dirty versions, bad rows and statements waiting on locks in its own
// database; whatever happens to it, the shared database stays as it was. Each case runs
// the real test in a child process (so t.Fatal, the cleanups and the binary's timeout are
// the real ones) while the parent holds the shared database's snapshot:
//   - the child fails in the middle, with 0009 dirty;
//   - the shared stocks table is locked by another session and the child has 2s in all;
//   - TEST_DATABASE_URL carries application_name and more query parameters.
//
// After each: the shared snapshot (version, dirty, CHECK, rows; non-empty before) is
// unchanged, the child's database is gone, and the next fixture can Up.
func TestStocksSKUCheck_IsolatedFromSharedDatabase(t *testing.T) {
	cfg := config.Postgres{URL: pgtest.URL(t)}
	full := postgres.Migration()
	c := context.Background()

	cases := []struct {
		name    string
		env     []string
		budget  string
		lock    bool
		params  string
		wantErr string // output must contain it; empty means the child passes
	}{
		{name: "fails halfway with 0009 dirty", env: []string{faultEnv + "=1"}, wantErr: faultMsg},
		{name: "shared stocks locked, 2s budget", budget: "2s", lock: true},
		{name: "application_name and other parameters", params: "application_name=skuprobe&connect_timeout=5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := platformpg.Up(c, cfg, full); err != nil {
				t.Fatal(err)
			}
			db := openDB(t, cfg.URL)
			if err := db.Exec(`INSERT INTO inventory.stocks (sku, on_hand, reserved, version, updated_at) VALUES ('sku-check-sentinel', 1, 0, 1, now())`).Error; err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Exec(`DELETE FROM inventory.stocks WHERE sku = 'sku-check-sentinel'`) })
			before := takeSnapshot(t, cfg, db)
			if before.Version != 9 || before.Dirty || before.Checks == 0 || before.Rows == 0 {
				t.Fatalf("the shared database is not a non-empty latest fixture to compare against: %+v", before)
			}

			name := "skucheck_" + randomName(t)
			t.Cleanup(func() {
				if err := dropDatabase(db, name); err != nil {
					t.Errorf("drop database %s: %v", name, err)
				}
			})
			var tx *gorm.DB
			if tc.lock {
				tx = db.Begin()
				if tx.Error != nil {
					t.Fatal(tx.Error)
				}
				t.Cleanup(func() { tx.Rollback() })
				if err := tx.Exec("LOCK TABLE inventory.stocks IN ACCESS EXCLUSIVE MODE").Error; err != nil {
					t.Fatal(err)
				}
			}

			childURL := cfg.URL
			if tc.params != "" {
				sep := "?"
				if strings.Contains(childURL, "?") {
					sep = "&"
				}
				childURL += sep + tc.params
			}
			var args []string
			if tc.budget != "" {
				args = append(args, "-test.timeout="+tc.budget)
			}
			out, err := runSelf(childURL, "^TestStocksSKUCheck_Postgres$", append(tc.env, dbNameEnv+"="+name), time.Minute, args...)
			if strings.Contains(string(out), "panic: test timed out") {
				t.Fatalf("the child ran into its binary timeout:\n%s", out)
			}
			var exit *exec.ExitError
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("the child should pass: %v\n%s", err, out)
			case tc.wantErr == "" && !strings.Contains(string(out), "--- PASS: TestStocksSKUCheck_Postgres"):
				t.Fatalf("the child did not run the test (exit 0 alone proves nothing):\n%s", out)
			case tc.wantErr != "" && !errors.As(err, &exit):
				t.Fatalf("the child should fail on %q, err=%v\n%s", tc.wantErr, err, out)
			case tc.wantErr != "" && !strings.Contains(string(out), tc.wantErr):
				t.Fatalf("the child failed, but not on %q:\n%s", tc.wantErr, out)
			}

			if tx != nil {
				if err := tx.Rollback().Error; err != nil {
					t.Fatal(err)
				}
			}
			if after := takeSnapshot(t, cfg, db); after != before {
				t.Fatalf("the shared database changed: before %+v, after %+v\n%s", before, after, out)
			}
			// The child's own cleanup dropped its database; if it was killed first, the
			// cleanup above does, and either way it must not be there at the end.
			if err := dropDatabase(db, name); err != nil {
				t.Fatal(err)
			}
			var left int
			if err := db.Raw(`SELECT count(*) FROM pg_database WHERE datname = ?`, name).Scan(&left).Error; err != nil || left != 0 {
				t.Fatalf("database %s left behind = %d, err=%v", name, left, err)
			}
			if err := platformpg.Up(c, cfg, full); err != nil {
				t.Fatalf("the next fixture could not Up: %v", err)
			}
		})
	}
}

// Without CREATEDB the test says so, with the server's reason; it neither skips nor
// falls back to the shared database.
func TestStocksSKUCheck_NeedsCreatedb(t *testing.T) {
	base := pgtest.URL(t)
	db := openDB(t, base)
	role := "skucheck_role_" + randomName(t)
	if err := db.Exec(fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD 'x' NOCREATEDB", role)).Error; err != nil {
		t.Fatalf("create role %s: %v (this check needs a role that can create roles)", role, err)
	}
	t.Cleanup(func() {
		if err := db.Exec("DROP ROLE IF EXISTS " + role).Error; err != nil {
			t.Errorf("drop role %s: %v", role, err)
		}
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, "x")

	out, err := runSelf(u.String(), "^TestStocksSKUCheck_Postgres$", nil, time.Minute)
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("a role without CREATEDB should fail the test, err=%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "42501") || !strings.Contains(string(out), "CREATEDB") {
		t.Fatalf("the failure does not carry SQLSTATE 42501 and the CREATEDB explanation:\n%s", out)
	}
}
