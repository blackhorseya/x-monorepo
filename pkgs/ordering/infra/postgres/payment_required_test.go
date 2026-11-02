package postgres_test

import (
	"context"
	"io/fs"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// Orders that already exist when the payment_required column arrives were CONFIRMED
// before Payment existed (docs/payment.md §2): the migration must leave every one of
// them false, whatever its status. The fixture is written BEFORE the migration runs:
// the schema is brought to version 8 (the last migration before payment_required) with
// a Migration whose files stop there, the legacy rows go in, and only then does the
// full migration set run over them. The driver runs each migration file in one
// transaction, so this is the same path a deployed database takes.
func TestMigration_PaymentRequired_ExistingOrdersAreFalse(t *testing.T) {
	url := pgtest.URL(t)

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
	t.Cleanup(cleanup) // registered first: Cleanup is LIFO, so the restore below runs while the pool is open

	full := postgres.Migration()
	upTo8 := platformpg.Migration{Schema: full.Schema, Files: filesUpTo(t, full.Files, 8)}

	restore := func() {
		// Leave the schema as every other test in the package expects it: fully migrated, empty.
		if err := platformpg.Up(c, cfg, full); err != nil {
			t.Errorf("restore: Up: %v", err)
			return
		}
		if err := db.WithContext(c).Exec(`TRUNCATE ordering.orders CASCADE`).Error; err != nil {
			t.Errorf("restore: truncate: %v", err)
		}
	}
	t.Cleanup(restore)

	// Start from an empty schema: drop it (the pgtest database is ours to wipe) and rebuild to version 8.
	if err := db.WithContext(c).Exec(`DROP SCHEMA IF EXISTS ordering CASCADE`).Error; err != nil {
		t.Fatal(err)
	}
	if err := platformpg.Up(c, cfg, upTo8); err != nil {
		t.Fatalf("Up to 8: %v", err)
	}
	if v, dirty, err := platformpg.Version(c, cfg, upTo8); err != nil || v != 8 || dirty {
		t.Fatalf("version before the migration under test = %d dirty=%v err=%v, want 8 clean", v, dirty, err)
	}

	// Legacy rows: one per status a pre-payment database can hold.
	now := time.Now().UTC().Truncate(time.Microsecond)
	statuses := []string{"PENDING", "CONFIRMED", "COMPLETED", "CANCELLED"}
	for n, status := range statuses {
		err := db.WithContext(c).Exec(
			`INSERT INTO ordering.orders (id, customer_id, status, created_at, updated_at, version) VALUES (?, 'c-legacy', ?, ?, ?, 1)`,
			"legacy-"+strconv.Itoa(n), status, now, now,
		).Error
		if err != nil {
			t.Fatalf("legacy fixture %s: %v", status, err)
		}
	}
	assertLegacyRows(t, db, len(statuses))

	// The migration under test.
	if err := platformpg.Up(c, cfg, full); err != nil {
		t.Fatalf("Up (full): %v", err)
	}

	var cols []struct {
		DataType      string
		IsNullable    string
		ColumnDefault *string
	}
	err = db.WithContext(c).Raw(
		`SELECT data_type, is_nullable, column_default FROM information_schema.columns WHERE table_schema = 'ordering' AND table_name = 'orders' AND column_name = 'payment_required'`,
	).Scan(&cols).Error
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 1 {
		t.Fatalf("ordering.orders.payment_required: %d columns after the migration, want 1", len(cols))
	}
	if cols[0].DataType != "boolean" || cols[0].IsNullable != "NO" || cols[0].ColumnDefault == nil || *cols[0].ColumnDefault != "false" {
		t.Fatalf("payment_required = %+v, want boolean NOT NULL DEFAULT false", cols[0])
	}

	var trues, total int64
	if err := db.WithContext(c).Raw(`SELECT count(*) FILTER (WHERE payment_required), count(*) FROM ordering.orders WHERE customer_id = 'c-legacy'`).Row().Scan(&trues, &total); err != nil {
		t.Fatal(err)
	}
	if total != int64(len(statuses)) || trues != 0 {
		t.Fatalf("legacy orders after the migration: %d rows, %d payment_required=true; want %d rows, 0 true", total, trues, len(statuses))
	}
}

func assertLegacyRows(t *testing.T, db *gorm.DB, want int) {
	t.Helper()
	var n int64
	if err := db.WithContext(context.Background()).Raw(`SELECT count(*) FROM ordering.orders WHERE customer_id = 'c-legacy'`).Scan(&n).Error; err != nil || n != int64(want) {
		t.Fatalf("legacy fixture rows = %d, %v; want %d (an empty fixture proves nothing)", n, err, want)
	}
}

// filesUpTo is the migration files whose numeric prefix is at most max.
func filesUpTo(t *testing.T, all fs.FS, max int) fs.FS {
	t.Helper()
	out := fstest.MapFS{}
	entries, err := fs.ReadDir(all, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		n, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0])
		if err != nil {
			t.Fatalf("migration file %q has no numeric prefix", e.Name())
		}
		if n > max {
			continue
		}
		data, err := fs.ReadFile(all, e.Name())
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = &fstest.MapFile{Data: data}
	}
	if len(out) == 0 {
		t.Fatal("no migration files selected")
	}

	return out
}
