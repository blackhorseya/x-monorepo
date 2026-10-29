package postgres_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/catalog/domain/product"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

func money(t *testing.T, amount int64, currency string) kernel.Money {
	t.Helper()
	m, err := kernel.NewMoney(amount, currency)
	if err != nil {
		t.Fatal(err)
	}

	return m
}

// Integration test: a throwaway Postgres container, or TEST_DATABASE_URL (see pgtest).
func TestProductRepository_Postgres(t *testing.T) {
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

	m := postgres.Migration()
	// Down → Up exercises the .down.sql files too; start from whatever state the DB is in.
	for _, step := range []func(context.Context, config.Postgres, platformpg.Migration) error{platformpg.Up, platformpg.Down, platformpg.Up} {
		if err := step(c, cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	if v, dirty, err := platformpg.Version(c, cfg, m); err != nil || v != 1 || dirty {
		t.Fatalf("version = %d dirty=%v err=%v, want 1 clean", v, dirty, err)
	}
	if err := db.WithContext(c).Exec("TRUNCATE catalog.products").Error; err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	products := postgres.NewProductRepository(db)

	// ── insert, reload ─────────────────────────────────────────────────────
	if _, err := products.FindBySKU(c, "A"); !errors.Is(err, product.ErrNotFound) {
		t.Fatalf("missing sku: %v", err)
	}
	a, _ := product.New("A", "Apple", money(t, 150, "USD"), now)
	if err := products.Save(c, a); err != nil {
		t.Fatal(err)
	}
	// Two writers both missed FindBySKU and created the SKU: the second insert must lose.
	twin, _ := product.New("A", "Twin", money(t, 1, "USD"), now)
	if err := products.Save(c, twin); !errors.Is(err, product.ErrStale) {
		t.Fatalf("second insert of the same sku: %v, want ErrStale", err)
	}
	got, err := products.FindBySKU(c, "A")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name() != "Apple" || got.Price() != money(t, 150, "USD") || !got.Active() || got.Version() != 1 || !got.UpdatedAt().Equal(now) {
		t.Fatalf("loaded = %+v", got)
	}

	// ── optimistic lock; a reprice is what the next read sees ──────────────
	other, _ := products.FindBySKU(c, "A")
	later := now.Add(time.Hour)
	if err := got.Reprice(money(t, 200, "EUR"), later); err != nil {
		t.Fatal(err)
	}
	if err := products.Save(c, got); err != nil {
		t.Fatal(err)
	}
	other.Deactivate(later)
	if err := products.Save(c, other); !errors.Is(err, product.ErrStale) {
		t.Fatalf("stale writer: %v, want ErrStale", err)
	}
	if again, _ := products.FindBySKU(c, "A"); again.Price() != money(t, 200, "EUR") || again.Version() != 2 || !again.Active() {
		t.Fatalf("after reprice = %+v, want 200 EUR, version 2, still active", again)
	}
	ghost, _ := product.Reconstitute(product.ReconstituteParams{SKU: "GHOST", Name: "Ghost", Price: money(t, 1, "USD"), Active: true, Version: 3, UpdatedAt: now})
	if err := products.Save(c, ghost); !errors.Is(err, product.ErrStale) {
		t.Fatalf("update of a missing row: %v, want ErrStale", err)
	}

	// ── FindBySKUs: one query, missing ones are absent ─────────────────────
	for _, sku := range []product.SKU{"B", "B-1", "B_1"} {
		p, _ := product.New(sku, "Name "+sku.String(), money(t, 10, "USD"), now)
		if err := products.Save(c, p); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := products.FindBySKU(c, "B")
	b.Deactivate(later)
	if err := products.Save(c, b); err != nil {
		t.Fatal(err)
	}
	many, err := products.FindBySKUs(c, []product.SKU{"A", "B", "NOPE", "A"})
	if err != nil || len(many) != 2 || many["A"] == nil || many["B"] == nil || many["B"].Active() || many["NOPE"] != nil {
		t.Fatalf("FindBySKUs = %v err=%v", many, err)
	}
	if many, err = products.FindBySKUs(c, nil); err != nil || len(many) != 0 {
		t.Fatalf("no skus: %v, %v", many, err)
	}

	// ── List: paging and a literal prefix ──────────────────────────────────
	page, next, err := products.List(c, product.ListParams{Limit: 2})
	if err != nil || len(page) != 2 || page[0].SKU() != "A" || page[1].SKU() != "B" || next != "B" {
		t.Fatalf("page 1 = %v next=%q err=%v", page, next, err)
	}
	page, next, err = products.List(c, product.ListParams{Limit: 2, After: next})
	if err != nil || len(page) != 2 || page[0].SKU() != "B-1" || page[1].SKU() != "B_1" || next != "" {
		t.Fatalf("page 2 = %v next=%q err=%v", page, next, err)
	}
	if page, _, err = products.List(c, product.ListParams{Limit: 10, SKUPrefix: "B_"}); err != nil || len(page) != 1 || page[0].SKU() != "B_1" {
		t.Fatalf("prefix with _ = %v err=%v (must not match B-1)", page, err)
	}
	if page, _, err = products.List(c, product.ListParams{SKUPrefix: "B"}); err != nil || len(page) != 3 {
		t.Fatalf("prefix B = %v err=%v (inactive products are listed too)", page, err)
	}

	// ── schema checks keep the domain's invariants even for a hand-written row
	if err := db.WithContext(c).Exec(`INSERT INTO catalog.products (sku, name, price_amount, price_currency, active, version, updated_at) VALUES ('NEG', 'x', -1, 'USD', true, 1, now())`).Error; err == nil {
		t.Fatal("negative amount accepted by the schema")
	}
	if err := db.WithContext(c).Exec(`INSERT INTO catalog.products (sku, name, price_amount, price_currency, active, version, updated_at) VALUES ('CUR', 'x', 1, 'usd', true, 1, now())`).Error; err == nil {
		t.Fatal("lower case currency accepted by the schema")
	}
}
