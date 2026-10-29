package postgres_test

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/catalog/domain/product"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

func migratedDB(t *testing.T) *gorm.DB {
	t.Helper()
	cfg := config.Postgres{URL: pgtest.URL(t)}
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(telCleanup)
	db, cleanup, err := platformpg.NewDB(cfg, tel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	if err := platformpg.Up(context.Background(), cfg, postgres.Migration()); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("TRUNCATE catalog.products").Error; err != nil {
		t.Fatal(err)
	}

	return db
}

const insertSQL = `INSERT INTO catalog.products (sku, name, price_amount, price_currency, active, version, updated_at) VALUES (?, ?, ?, ?, true, 1, now())`

// The schema refuses what the aggregate refuses, or a hand-written row would
// make GetProduct / ListProducts fail on a request that is fine.
func TestSchema_AgreesWithTheDomain(t *testing.T) {
	db := migratedDB(t)

	// names: every blank the domain refuses is refused by the CHECK too
	blanks := []string{"\t", "\n", "\v", "\f", "\r", " ", "\u0085", " ", " ", " ", " ", " ", " ", " ", "　", " \t　 "}
	for n, name := range blanks {
		if err := db.Exec(insertSQL, "BLANK", name, 1, "USD").Error; err == nil {
			t.Errorf("blank name %q (case %d) accepted by the schema", name, n)
			db.Exec("DELETE FROM catalog.products")
		}
	}
	if err := db.Exec(insertSQL, "ZW", "​", 1, "USD").Error; err != nil {
		t.Errorf("zero width space is not whitespace to the domain, the schema refused it: %v", err)
	}

	// currencies: only assigned codes
	for _, c := range []string{"AAA", "ZZZ", "QQQ"} {
		if err := db.Exec(insertSQL, "CUR-"+c, "x", 1, c).Error; err == nil {
			t.Errorf("unassigned currency %s accepted by the schema", c)
		}
	}
	for _, c := range []string{"USD", "EUR", "JPY", "TWD"} {
		if err := db.Exec(insertSQL, "CUR-"+c, "x", 1, c).Error; err != nil {
			t.Errorf("assigned currency %s refused by the schema: %v", c, err)
		}
	}
}

// Keyset paging and prefix search must not depend on the database's locale:
// the memory repository orders bytewise, so the sku column must too. In an
// en_US cluster the default collation would put "B_1" before "B-1".
func TestSchema_SKUOrderIsBytewise(t *testing.T) {
	db := migratedDB(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	repo := postgres.NewProductRepository(db)
	c := context.Background()
	for _, sku := range []product.SKU{"b", "B_1", "A", "B-1", "B", "a"} {
		m := money(t, 1, "USD")
		p, _ := product.New(sku, "Name "+sku.String(), m, now)
		if err := repo.Save(c, p); err != nil {
			t.Fatal(err)
		}
	}

	var got []product.SKU
	var after product.SKU
	for {
		page, next, err := repo.List(c, product.ListParams{Limit: 2, After: after})
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range page {
			got = append(got, p.SKU())
		}
		if next == "" {
			break
		}
		after = next
	}
	want := []product.SKU{"A", "B", "B-1", "B_1", "a", "b"}
	if len(got) != len(want) {
		t.Fatalf("paged %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("paged %v, want %v (bytewise)", got, want)
		}
	}
}

// The Go whitelist the aggregate checks and the table the foreign key checks are one list.
func TestSchema_CurrencySeedMatchesGo(t *testing.T) {
	db := migratedDB(t)

	var seeded []string
	if err := db.Raw(`SELECT code FROM catalog.currencies ORDER BY code COLLATE "C"`).Scan(&seeded).Error; err != nil {
		t.Fatal(err)
	}
	want := product.CurrencyCodes()
	if len(seeded) != len(want) {
		t.Fatalf("seeded %d codes, Go has %d", len(seeded), len(want))
	}
	for i := range want {
		if seeded[i] != want[i] {
			t.Fatalf("code %d: seed %q, Go %q", i, seeded[i], want[i])
		}
	}
	for _, c := range want { // and every one of them is usable as a price currency
		if err := db.Exec(insertSQL, "CUR-"+c, "x", 1, c).Error; err != nil {
			t.Errorf("seeded currency %s refused by the foreign key: %v", c, err)
		}
	}
}
