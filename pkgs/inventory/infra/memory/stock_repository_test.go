package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/stock"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/memory"
)

// The memory repository backs `task run` and the rpc E2E, so it must enforce
// the same Save contract as Postgres or nothing local catches a regression.
func TestStockRepository_Versioning(t *testing.T) {
	c := context.Background()
	repo := memory.NewStockRepository()
	now := time.Now()

	first, _ := stock.New("A", 5, now)
	twin, _ := stock.New("A", 42, now)
	if err := repo.Save(c, first); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := repo.Save(c, twin); !errors.Is(err, stock.ErrStale) {
		t.Fatalf("second fresh insert: err = %v, want ErrStale", err)
	}

	a, _ := repo.FindBySKU(c, "A")
	b, _ := repo.FindBySKU(c, "A")
	if a.Version() != 1 || a.OnHand() != 5 {
		t.Fatalf("loaded: version=%d on_hand=%d, want 1 and 5", a.Version(), a.OnHand())
	}
	_ = a.Restock(6, now)
	_ = b.Restock(7, now)
	if err := repo.Save(c, a); err != nil {
		t.Fatalf("first writer: %v", err)
	}
	if err := repo.Save(c, b); !errors.Is(err, stock.ErrStale) {
		t.Fatalf("second writer: err = %v, want ErrStale", err)
	}
	got, _ := repo.FindBySKU(c, "A")
	if got.Version() != 2 || got.OnHand() != 6 {
		t.Fatalf("stored: version=%d on_hand=%d, want 2 and 6", got.Version(), got.OnHand())
	}

	ghost, _ := stock.Reconstitute(stock.ReconstituteParams{SKU: "B", OnHand: 1, Version: 1, UpdatedAt: now})
	if err := repo.Save(c, ghost); !errors.Is(err, stock.ErrStale) {
		t.Fatalf("update of a missing sku: err = %v, want ErrStale", err)
	}
}

func TestStockRepository_List(t *testing.T) {
	c := context.Background()
	repo := memory.NewStockRepository()
	now := time.Now()
	for _, sku := range []string{"B", "C", "A"} {
		s, _ := stock.New(stock.SKU(sku), 1, now)
		if err := repo.Save(c, s); err != nil {
			t.Fatal(err)
		}
	}
	page, next, err := repo.List(c, stock.ListParams{Limit: 2})
	if err != nil || len(page) != 2 || page[0].SKU() != "A" || page[1].SKU() != "B" || next != "B" {
		t.Fatalf("page 1 = %v next=%q err=%v", page, next, err)
	}
	page, next, err = repo.List(c, stock.ListParams{Limit: 2, After: next})
	if err != nil || len(page) != 1 || page[0].SKU() != "C" || next != "" {
		t.Fatalf("page 2 = %v next=%q err=%v", page, next, err)
	}
	// Copies: mutating a listed aggregate does not touch the store.
	_ = page[0].Restock(99, now)
	if got, _ := repo.FindBySKU(c, "C"); got.OnHand() != 1 {
		t.Fatal("List returned an alias of the stored aggregate")
	}
	// Filters compose with the keyset: prefix, reserved > 0, and both.
	held, _ := stock.New("BA", 2, now)
	if short := stock.ReserveAll(map[string]*stock.Stock{"BA": held}, []string{"BA"}, map[string]int32{"BA": 1}, now); short != nil {
		t.Fatalf("reserve: short %v", short)
	}
	if err := repo.Save(c, held); err != nil {
		t.Fatal(err)
	}
	if page, next, err := repo.List(c, stock.ListParams{Limit: 10, SKUPrefix: "B"}); err != nil || len(page) != 2 || page[0].SKU() != "B" || page[1].SKU() != "BA" || next != "" {
		t.Fatalf("prefix = %v next=%q err=%v", page, next, err)
	}
	if page, _, err := repo.List(c, stock.ListParams{Limit: 10, OnlyReserved: true}); err != nil || len(page) != 1 || page[0].SKU() != "BA" {
		t.Fatalf("only reserved = %v err=%v", page, err)
	}
	if page, _, err := repo.List(c, stock.ListParams{Limit: 10, SKUPrefix: "A", OnlyReserved: true}); err != nil || len(page) != 0 {
		t.Fatalf("prefix + reserved = %v err=%v", page, err)
	}
}
