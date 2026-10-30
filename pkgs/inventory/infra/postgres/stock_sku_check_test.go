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

	full := postgres.Migration()
	before := upTo(t, full, 8)
	t.Cleanup(func() {
		db.Exec("TRUNCATE inventory.stocks CASCADE")
		_ = platformpg.Up(context.Background(), cfg, full)
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

	// An over-long SKU from before the rule blocks the migration.
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
