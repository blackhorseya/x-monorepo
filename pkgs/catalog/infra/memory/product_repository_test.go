package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/catalog/domain/product"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

func price(t *testing.T, amount int64) kernel.Money {
	t.Helper()
	m, err := kernel.NewMoney(amount, "USD")
	if err != nil {
		t.Fatal(err)
	}

	return m
}

// The memory repository backs `task run` and the rpc E2E, so it must enforce
// the same Save contract as Postgres or nothing local catches a regression.
func TestProductRepository_Versioning(t *testing.T) {
	c := context.Background()
	repo := memory.NewProductRepository()
	now := time.Now()

	if _, err := repo.FindBySKU(c, "A"); !errors.Is(err, product.ErrNotFound) {
		t.Fatalf("missing sku: %v", err)
	}

	first, _ := product.New("A", "Apple", price(t, 5), now)
	twin, _ := product.New("A", "Other", price(t, 42), now)
	if err := repo.Save(c, first); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := repo.Save(c, twin); !errors.Is(err, product.ErrStale) {
		t.Fatalf("second fresh insert: err = %v, want ErrStale", err)
	}

	a, _ := repo.FindBySKU(c, "A")
	b, _ := repo.FindBySKU(c, "A")
	if a.Version() != 1 || a.Price() != price(t, 5) {
		t.Fatalf("loaded: version=%d price=%v, want 1 and 5", a.Version(), a.Price())
	}
	_ = a.Reprice(price(t, 6), now)
	_ = b.Reprice(price(t, 7), now)
	if err := repo.Save(c, a); err != nil {
		t.Fatalf("first writer: %v", err)
	}
	if err := repo.Save(c, b); !errors.Is(err, product.ErrStale) {
		t.Fatalf("second writer: err = %v, want ErrStale", err)
	}
	got, _ := repo.FindBySKU(c, "A")
	if got.Version() != 2 || got.Price() != price(t, 6) {
		t.Fatalf("stored: version=%d price=%v, want 2 and 6", got.Version(), got.Price())
	}

	ghost, _ := product.Reconstitute(product.ReconstituteParams{SKU: "B", Name: "Ghost", Price: price(t, 1), Active: true, Version: 1, UpdatedAt: now})
	if err := repo.Save(c, ghost); !errors.Is(err, product.ErrStale) {
		t.Fatalf("update of a missing sku: err = %v, want ErrStale", err)
	}

	// what was loaded is a copy: changing it without Save does not leak into the store
	got.Deactivate(now)
	if again, _ := repo.FindBySKU(c, "A"); !again.Active() {
		t.Fatal("an unsaved change is visible through the repository")
	}
}

func TestProductRepository_FindBySKUs(t *testing.T) {
	c := context.Background()
	repo := memory.NewProductRepository()
	now := time.Now()
	for _, sku := range []product.SKU{"A", "B"} {
		p, _ := product.New(sku, "Name "+sku.String(), price(t, 1), now)
		if err := repo.Save(c, p); err != nil {
			t.Fatal(err)
		}
	}

	got, err := repo.FindBySKUs(c, []product.SKU{"A", "Z", "B", "A"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["A"] == nil || got["B"] == nil || got["Z"] != nil {
		t.Fatalf("FindBySKUs = %v, want A and B only (a missing sku is absent, not an error)", got)
	}
	if got, err = repo.FindBySKUs(c, nil); err != nil || len(got) != 0 {
		t.Fatalf("no skus: %v, %v", got, err)
	}
}

func TestProductRepository_List(t *testing.T) {
	c := context.Background()
	repo := memory.NewProductRepository()
	now := time.Now()
	for _, sku := range []product.SKU{"B_1", "B-1", "A", "B"} {
		p, _ := product.New(sku, "Name "+sku.String(), price(t, 1), now)
		if err := repo.Save(c, p); err != nil {
			t.Fatal(err)
		}
	}

	page, next, err := repo.List(c, product.ListParams{Limit: 2})
	if err != nil || len(page) != 2 || page[0].SKU() != "A" || page[1].SKU() != "B" || next != "B" {
		t.Fatalf("page 1 = %v next=%q err=%v", page, next, err)
	}
	page, next, err = repo.List(c, product.ListParams{Limit: 2, After: next})
	if err != nil || len(page) != 2 || page[0].SKU() != "B-1" || page[1].SKU() != "B_1" || next != "" {
		t.Fatalf("page 2 = %v next=%q err=%v", page, next, err)
	}
	if page, _, err = repo.List(c, product.ListParams{Limit: 10, SKUPrefix: "B_"}); err != nil || len(page) != 1 || page[0].SKU() != "B_1" {
		t.Fatalf("literal prefix = %v err=%v", page, err)
	}
	if page, _, err = repo.List(c, product.ListParams{SKUPrefix: "Z"}); err != nil || len(page) != 0 {
		t.Fatalf("no match = %v err=%v", page, err)
	}
}
