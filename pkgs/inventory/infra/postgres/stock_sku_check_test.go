package postgres_test

import (
	"context"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// upTo keeps the migration files up to and including version n.
func upTo(t skuT, m platformpg.Migration, n int) platformpg.Migration {
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
	stocksSKUCheck(t, pgtest.URL(t), nil)
}

// A failure between the dirty 0009 and the documented reset must not leave the shared
// database dirty for the next migration test of the process. The failure is injected
// into the real test body; once its cleanups have run the schema has to be at the
// latest version, clean, with the over-long row gone and the CHECK back.
func TestStocksSKUCheck_FailureLeavesCleanDatabase(t *testing.T) {
	url := pgtest.URL(t)
	cfg := config.Postgres{URL: url}
	full := postgres.Migration()

	rec := &recT{}
	rec.run(func() {
		stocksSKUCheck(rec, url, func() { rec.Fatal("injected failure while 0009 is dirty") })
	})
	if len(rec.fatals) != 1 || rec.fatals[0] != "injected failure while 0009 is dirty" {
		t.Fatalf("the injection did not stop the body as the only failure: %q", rec.fatals)
	}
	if len(rec.errors) > 0 {
		t.Fatalf("restore reported errors: %q", rec.errors)
	}

	c := context.Background()
	v, dirty, err := platformpg.Version(c, cfg, full)
	if err != nil || v != 9 || dirty {
		t.Fatalf("after the failed body: version = %d dirty=%v err=%v, want 9 clean", v, dirty, err)
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
	var rows, checks int
	if err := db.Raw(`SELECT count(*) FROM inventory.stocks`).Scan(&rows).Error; err != nil || rows != 0 {
		t.Fatalf("stocks rows = %d, err=%v; want the injected row gone", rows, err)
	}
	if err := db.Raw(`SELECT count(*) FROM pg_constraint WHERE conrelid = 'inventory.stocks'::regclass AND contype = 'c' AND pg_get_constraintdef(oid) ILIKE '%octet_length%'`).Scan(&checks).Error; err != nil || checks == 0 {
		t.Fatalf("CHECK count = %d, err=%v; want 0009's CHECK back", checks, err)
	}
	if err := platformpg.Up(c, cfg, full); err != nil {
		t.Fatalf("the next migration test could not Up: %v", err)
	}
}

// skuT is the slice of testing.TB the body uses, so a test can drive it with a recorder
// and see what its cleanups leave behind after a failure.
type skuT interface {
	Helper()
	Fatal(args ...any)
	Fatalf(format string, args ...any)
	Errorf(format string, args ...any)
	Cleanup(f func())
}

// recT records failures instead of failing; Fatal* stop the running function the way
// FailNow does, and the cleanups run LIFO like testing's.
type recT struct {
	fatals   []string
	errors   []string
	cleanups []func()
}

type recStop struct{}

func (r *recT) Helper()                   {}
func (r *recT) Cleanup(f func())          { r.cleanups = append(r.cleanups, f) }
func (r *recT) Errorf(f string, a ...any) { r.errors = append(r.errors, fmt.Sprintf(f, a...)) }
func (r *recT) Fatalf(f string, a ...any) { r.Fatal(fmt.Sprintf(f, a...)) }
func (r *recT) Fatal(a ...any) {
	r.fatals = append(r.fatals, fmt.Sprint(a...))
	panic(recStop{})
}

func (r *recT) run(fn func()) {
	guard := func(f func()) {
		defer func() {
			if v := recover(); v != nil {
				if _, ok := v.(recStop); !ok {
					panic(v)
				}
			}
		}()
		f()
	}
	guard(fn)
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		guard(r.cleanups[i])
	}
}

func stocksSKUCheck(t skuT, url string, afterDirty func()) {
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
	// Leave the shared database at the latest version, clean, whatever happened above.
	// A failure after the dirty 0009 would otherwise hand the next migration test a
	// dirty version, an over-long row and no CHECK. Errors fail the test.
	t.Cleanup(func() {
		ctx := context.Background()
		v, dirty, err := platformpg.Version(ctx, cfg, full)
		if err != nil {
			t.Errorf("restore: read version: %v", err)

			return
		}
		if dirty {
			// The failed migration left nothing behind, so the previous version is the truth.
			if err := db.WithContext(ctx).Exec("UPDATE inventory.schema_migrations SET version = ?, dirty = false", v-1).Error; err != nil {
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
	})

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
