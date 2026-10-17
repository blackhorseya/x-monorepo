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
