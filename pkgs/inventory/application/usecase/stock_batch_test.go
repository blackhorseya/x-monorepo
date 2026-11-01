package usecase_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/stock"
)

// recordingStocks wraps a stock.Repository and records how it was used: how many
// reads reached it (single-SKU or batch) and in which order stocks were saved.
// Its batch read answers from the wrapped single-SKU reads, so the same double
// serves before and after the repository grew a batch method.
type recordingStocks struct {
	stock.Repository
	singleReads int
	batchReads  int
	saved       []string
}

func (x *recordingStocks) FindBySKU(c context.Context, sku stock.SKU) (*stock.Stock, error) {
	x.singleReads++

	return x.Repository.FindBySKU(c, sku)
}

func (x *recordingStocks) FindBySKUs(c context.Context, skus []stock.SKU) ([]*stock.Stock, error) {
	x.batchReads++
	var out []*stock.Stock
	for _, sku := range skus {
		s, err := x.Repository.FindBySKU(c, sku)
		if err != nil {
			continue // never stocked: left out
		}
		out = append(out, s)
	}

	return out, nil
}

func (x *recordingStocks) Save(c context.Context, s *stock.Stock) error {
	x.saved = append(x.saved, s.SKU().String())

	return x.Repository.Save(c, s)
}

func (x *recordingStocks) reset() { x.singleReads, x.batchReads, x.saved = 0, 0, nil }

// skusDescending is 20 SKUs handed over highest first, so any order that is not
// "ascending by SKU" (input order, map order) is visibly different.
func skusDescending() []string {
	skus := make([]string, 0, 20)
	for n := 19; n >= 0; n-- {
		skus = append(skus, fmt.Sprintf("SKU-%02d", n))
	}

	return skus
}

func newBatchFixture(t *testing.T, skus []string) (*fixture, *recordingStocks) {
	t.Helper()
	f := newFixture()
	rec := &recordingStocks{Repository: f.stocks}
	f.reserve = usecase.NewReserveStock(f.reservations, rec, f.rec, f.clock)
	f.release = usecase.NewReleaseStock(f.reservations, rec, f.rec, f.clock)
	f.commit = usecase.NewCommitStock(f.reservations, rec, f.rec, f.clock)
	f.expire = usecase.NewExpireReservations(f.reservations, rec, f.rec, f.clock)
	for _, sku := range skus {
		if _, err := f.setStock.Execute(context.Background(), dto.SetStockInput{SKU: sku, OnHand: 10}); err != nil {
			t.Fatal(err)
		}
	}

	return f, rec
}

func linesOf(skus []string) []dto.Line {
	lines := make([]dto.Line, 0, len(skus))
	for _, sku := range skus {
		lines = append(lines, dto.Line{SKU: sku, Quantity: 1})
	}

	return lines
}

// Loading the stocks of an order is one repository read, however many lines it
// has; a SKU repeated across lines is read once, and a SKU that was never
// stocked is simply absent (the domain counts it as zero available).
func TestStockUseCases_LoadStocksInOneRead(t *testing.T) {
	c := context.Background()
	skus := skusDescending()

	t.Run("reserve", func(t *testing.T) {
		f, rec := newBatchFixture(t, skus)
		lines := append(linesOf(skus), dto.Line{SKU: skus[0], Quantity: 1}) // repeated SKU
		rec.reset()

		out, err := f.reserve.Execute(c, dto.ReserveStockInput{OrderID: "o-1", Lines: lines})
		if err != nil || out.Status != "RESERVED" {
			t.Fatalf("reserve = %+v, %v; want RESERVED", out, err)
		}
		if rec.singleReads != 0 || rec.batchReads != 1 {
			t.Fatalf("reads: single=%d batch=%d, want one batch read and no single reads", rec.singleReads, rec.batchReads)
		}
		if len(rec.saved) != len(skus) {
			t.Fatalf("saved %d stocks, want %d (one per distinct SKU)", len(rec.saved), len(skus))
		}
	})

	t.Run("reserve with a SKU that was never stocked", func(t *testing.T) {
		f, rec := newBatchFixture(t, skus)
		rec.reset()

		out, err := f.reserve.Execute(c, dto.ReserveStockInput{OrderID: "o-2", Lines: append(linesOf(skus[:3]), dto.Line{SKU: "NEVER-STOCKED", Quantity: 1})})
		if err != nil || out.Status != "REJECTED" {
			t.Fatalf("reserve = %+v, %v; want REJECTED", out, err)
		}
		if rec.singleReads != 0 || rec.batchReads != 1 {
			t.Fatalf("reads: single=%d batch=%d, want one batch read and no single reads", rec.singleReads, rec.batchReads)
		}
		if len(rec.saved) != 0 {
			t.Fatalf("a rejected reservation saved stocks: %v", rec.saved)
		}
	})

	for _, tc := range []struct {
		name string
		run  func(f *fixture) error
	}{
		{"release", func(f *fixture) error {
			_, err := f.release.Execute(c, dto.ReleaseStockInput{OrderID: "o-1"})
			return err
		}},
		{"commit", func(f *fixture) error {
			_, err := f.commit.Execute(c, dto.CommitStockInput{OrderID: "o-1"})
			return err
		}},
		{"expire", func(f *fixture) error {
			_, err := f.expire.Execute(c, dto.ExpireReservationsInput{OlderThan: time.Hour})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, rec := newBatchFixture(t, skus)
			// Reserved an hour and a bit ago, so the expire sweep finds it.
			past := usecase.NewReserveStock(f.reservations, rec, f.rec, fixedClock{t: f.clock.t.Add(-2 * time.Hour)})
			if out, err := past.Execute(c, dto.ReserveStockInput{OrderID: "o-1", Lines: linesOf(skus)}); err != nil || out.Status != "RESERVED" {
				t.Fatalf("setup reserve = %+v, %v", out, err)
			}
			rec.reset()

			if err := tc.run(f); err != nil {
				t.Fatal(err)
			}
			if rec.singleReads != 0 || rec.batchReads != 1 {
				t.Fatalf("reads: single=%d batch=%d, want one batch read and no single reads", rec.singleReads, rec.batchReads)
			}
			if len(rec.saved) != len(skus) {
				t.Fatalf("saved %d stocks, want %d", len(rec.saved), len(skus))
			}
		})
	}
}

// Stocks are written in SKU order whatever order the lines came in, so two
// transactions that share SKUs touch the rows in the same sequence. This covers
// the writes of one call only; a sweep handling several reservations in one
// transaction (expire) orders each reservation's stocks, not the sweep.
func TestStockUseCases_SaveStocksInSKUOrder(t *testing.T) {
	c := context.Background()
	skus := skusDescending()
	want := slices.Sorted(slices.Values(skus))

	for _, tc := range []struct {
		name string
		run  func(f *fixture) error
	}{
		{"reserve", func(f *fixture) error {
			_, err := f.reserve.Execute(c, dto.ReserveStockInput{OrderID: "o-fresh", Lines: linesOf(skus)})
			return err
		}},
		{"release", func(f *fixture) error {
			_, err := f.release.Execute(c, dto.ReleaseStockInput{OrderID: "o-1"})
			return err
		}},
		{"commit", func(f *fixture) error {
			_, err := f.commit.Execute(c, dto.CommitStockInput{OrderID: "o-1"})
			return err
		}},
		{"expire", func(f *fixture) error {
			_, err := f.expire.Execute(c, dto.ExpireReservationsInput{OlderThan: time.Hour})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, rec := newBatchFixture(t, skus)
			past := usecase.NewReserveStock(f.reservations, rec, f.rec, fixedClock{t: f.clock.t.Add(-2 * time.Hour)})
			if out, err := past.Execute(c, dto.ReserveStockInput{OrderID: "o-1", Lines: linesOf(skus)}); err != nil || out.Status != "RESERVED" {
				t.Fatalf("setup reserve = %+v, %v", out, err)
			}
			rec.reset()

			if err := tc.run(f); err != nil {
				t.Fatal(err)
			}
			if len(rec.saved) != len(skus) { // an empty list is trivially sorted
				t.Fatalf("saved %d stocks, want %d", len(rec.saved), len(skus))
			}
			if !slices.Equal(rec.saved, want) {
				t.Fatalf("save order = %v, want ascending %v", rec.saved, want)
			}
		})
	}
}
