package stock_test

import (
	"errors"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/stock"
)

var now = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func TestNew(t *testing.T) {
	s, err := stock.New("A", 10, now)
	if err != nil {
		t.Fatal(err)
	}
	if s.SKU() != "A" || s.SKU().String() != "A" || s.OnHand() != 10 || s.Reserved() != 0 || s.Available() != 10 || s.Version() != 0 || !s.UpdatedAt().Equal(now) {
		t.Fatalf("unexpected stock %+v", s)
	}

	if _, err := stock.New("", 1, now); !errors.Is(err, stock.ErrInvalidSKU) {
		t.Fatalf("empty sku: %v", err)
	}
	if _, err := stock.New("A", -1, now); !errors.Is(err, stock.ErrNegativeQuantity) {
		t.Fatalf("negative: %v", err)
	}
}

func TestReconstitute(t *testing.T) {
	s, err := stock.Reconstitute(stock.ReconstituteParams{SKU: "A", OnHand: 5, Reserved: 2, Version: 7, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if s.Available() != 3 || s.Version() != 7 {
		t.Fatalf("available=%d version=%d", s.Available(), s.Version())
	}

	cases := map[string]stock.ReconstituteParams{
		"empty sku":          {SKU: "", OnHand: 1},
		"negative on hand":   {SKU: "A", OnHand: -1},
		"negative reserved":  {SKU: "A", OnHand: 1, Reserved: -1},
		"reserved > on hand": {SKU: "A", OnHand: 1, Reserved: 2},
	}
	for name, p := range cases {
		_, err := stock.Reconstitute(p)
		want := stock.ErrCorruptQuantities
		if p.SKU == "" {
			want = stock.ErrInvalidSKU
		}
		if !errors.Is(err, want) {
			t.Errorf("%s: got %v, want %v", name, err, want)
		}
	}
}

func TestRestock(t *testing.T) {
	s, _ := stock.Reconstitute(stock.ReconstituteParams{SKU: "A", OnHand: 5, Reserved: 3, UpdatedAt: now})
	later := now.Add(time.Minute)

	if err := s.Restock(-1, later); !errors.Is(err, stock.ErrNegativeQuantity) {
		t.Fatalf("negative: %v", err)
	}
	if err := s.Restock(2, later); !errors.Is(err, stock.ErrBelowReserved) {
		t.Fatalf("below reserved: %v", err)
	}
	if err := s.Restock(3, later); err != nil {
		t.Fatal(err)
	}
	if s.OnHand() != 3 || s.Available() != 0 || !s.UpdatedAt().Equal(later) {
		t.Fatalf("on_hand=%d available=%d updated=%v", s.OnHand(), s.Available(), s.UpdatedAt())
	}
}

func TestReserveAll_AllOrNothing(t *testing.T) {
	a, _ := stock.New("A", 5, now)
	b, _ := stock.New("B", 1, now)
	stocks := map[string]*stock.Stock{"A": a, "B": b}
	later := now.Add(time.Minute)

	// B short, C unknown: nothing touched, both reported in the order asked.
	short := stock.ReserveAll(stocks, []string{"A", "B", "C"}, map[string]int32{"A": 2, "B": 2, "C": 1}, later)
	if len(short) != 2 || short[0] != "B" || short[1] != "C" {
		t.Fatalf("short = %v, want [B C]", short)
	}
	if a.Reserved() != 0 || b.Reserved() != 0 || !a.UpdatedAt().Equal(now) {
		t.Fatalf("stocks touched on shortfall: A=%d B=%d", a.Reserved(), b.Reserved())
	}

	// Zero quantity is never reservable.
	if short := stock.ReserveAll(stocks, []string{"A"}, map[string]int32{"A": 0}, later); len(short) != 1 {
		t.Fatalf("zero quantity: short = %v", short)
	}

	if short := stock.ReserveAll(stocks, []string{"A", "B"}, map[string]int32{"A": 2, "B": 1}, later); short != nil {
		t.Fatalf("short = %v, want none", short)
	}
	if a.Reserved() != 2 || a.Available() != 3 || b.Available() != 0 || !b.UpdatedAt().Equal(later) {
		t.Fatalf("A=%d/%d B=%d", a.Reserved(), a.Available(), b.Available())
	}
	if !a.CanReserve(3) || a.CanReserve(4) {
		t.Fatal("CanReserve must compare against available")
	}
}

func TestCommit(t *testing.T) {
	s, _ := stock.Reconstitute(stock.ReconstituteParams{SKU: "A", OnHand: 5, Reserved: 2, UpdatedAt: now})
	later := now.Add(time.Minute)

	if err := s.Commit(0, later); !errors.Is(err, stock.ErrInvalidQuantity) {
		t.Fatalf("zero: %v", err)
	}
	if err := s.Commit(3, later); !errors.Is(err, stock.ErrCommitExceedsReserved) {
		t.Fatalf("too much: %v", err)
	}
	if err := s.Commit(2, later); err != nil {
		t.Fatal(err)
	}
	// Sold: on-hand and reserved both drop, available is unchanged.
	if s.OnHand() != 3 || s.Reserved() != 0 || s.Available() != 3 || !s.UpdatedAt().Equal(later) {
		t.Fatalf("on_hand=%d reserved=%d available=%d", s.OnHand(), s.Reserved(), s.Available())
	}
}

func TestSellAll(t *testing.T) {
	a, _ := stock.Reconstitute(stock.ReconstituteParams{SKU: "A", OnHand: 5, Reserved: 2, UpdatedAt: now})
	b, _ := stock.Reconstitute(stock.ReconstituteParams{SKU: "B", OnHand: 1, Reserved: 1, UpdatedAt: now})
	stocks := map[string]*stock.Stock{"A": a, "B": b}
	later := now.Add(time.Minute)

	// Only what is available can be sold, and nothing moves unless every sku is covered:
	// B has nothing available, C is missing altogether.
	short := stock.SellAll(stocks, []string{"A", "B", "C"}, map[string]int32{"A": 3, "B": 1, "C": 1}, later)
	if len(short) != 2 || short[0] != "B" || short[1] != "C" {
		t.Fatalf("short = %v", short)
	}
	if a.OnHand() != 5 || a.Reserved() != 2 || b.OnHand() != 1 || !a.UpdatedAt().Equal(now) {
		t.Fatalf("failed sale must not change anything: A=%d/%d B=%d/%d", a.OnHand(), a.Reserved(), b.OnHand(), b.Reserved())
	}

	if short := stock.SellAll(stocks, []string{"A"}, map[string]int32{"A": 3}, later); short != nil {
		t.Fatalf("short = %v", short)
	}
	// Sold from what was available; the reserved part still belongs to others.
	if a.OnHand() != 2 || a.Reserved() != 2 || a.Available() != 0 || !a.UpdatedAt().Equal(later) {
		t.Fatalf("A on_hand=%d reserved=%d available=%d", a.OnHand(), a.Reserved(), a.Available())
	}
}

func TestRelease(t *testing.T) {
	s, _ := stock.Reconstitute(stock.ReconstituteParams{SKU: "A", OnHand: 5, Reserved: 2, UpdatedAt: now})
	later := now.Add(time.Minute)

	if err := s.Release(0, later); !errors.Is(err, stock.ErrInvalidQuantity) {
		t.Fatalf("zero: %v", err)
	}
	if err := s.Release(3, later); !errors.Is(err, stock.ErrReleaseExceedsReserved) {
		t.Fatalf("too much: %v", err)
	}
	if err := s.Release(2, later); err != nil {
		t.Fatal(err)
	}
	if s.Reserved() != 0 || s.Available() != 5 || !s.UpdatedAt().Equal(later) {
		t.Fatalf("reserved=%d available=%d", s.Reserved(), s.Available())
	}
}
