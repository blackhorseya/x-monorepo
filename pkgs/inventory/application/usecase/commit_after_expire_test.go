package usecase_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/stock"
	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

// reserveAt reserves one order as if the clock read at, then restores the fixture's clock.
func reserveAt(t *testing.T, f *fixture, at time.Time, orderID string, qty int32) {
	t.Helper()
	uc := usecase.NewReserveStock(f.reservations, f.stocks, f.rec, fixedClock{t: at})
	if _, err := uc.Execute(context.Background(), dto.ReserveStockInput{OrderID: orderID, Lines: []dto.Line{{SKU: "A", Quantity: qty}}}); err != nil {
		t.Fatal(err)
	}
}

func sweep(t *testing.T, f *fixture) {
	t.Helper()
	out, err := f.expire.Execute(context.Background(), dto.ExpireReservationsInput{OlderThan: 24 * time.Hour})
	if err != nil || len(out.Expired) != 1 {
		t.Fatalf("sweep = %+v, %v", out, err)
	}
}

// The window of issue #218: the sweep expires a reservation whose order has
// already been completed. OrderCompleted must still sell the stock when it can
// be covered, exactly once.
func TestCommitStock_AfterExpire_Recommits(t *testing.T) {
	c := context.Background()
	f := newFixture()
	if _, err := f.setStock.Execute(c, dto.SetStockInput{SKU: "A", OnHand: 5}); err != nil {
		t.Fatal(err)
	}
	reserveAt(t, f, f.clock.t.Add(-25*time.Hour), "o-1", 2)
	sweep(t, f)
	f.rec.events = nil

	out, err := f.commit.Execute(c, dto.CommitStockInput{OrderID: "o-1"})
	if err != nil || out.Status != "COMMITTED" {
		t.Fatalf("commit after expire = %+v, %v; want COMMITTED", out, err)
	}
	a, _ := f.getStock.Execute(c, dto.GetStockInput{SKU: "A"})
	if a.OnHand != 3 || a.Reserved != 0 || a.Available != 3 {
		t.Fatalf("after recommit A=%+v, want on_hand 3 reserved 0", a)
	}
	if len(f.rec.events) != 1 {
		t.Fatalf("events = %d, want Committed", len(f.rec.events))
	}
	if _, ok := f.rec.events[0].(reservation.Committed); !ok {
		t.Fatalf("event = %T", f.rec.events[0])
	}

	// A redelivered OrderCompleted must not sell twice.
	if _, err := f.commit.Execute(c, dto.CommitStockInput{OrderID: "o-1"}); !errors.Is(err, reservation.ErrNotReserved) {
		t.Fatalf("redelivery: %v", err)
	}
	a, _ = f.getStock.Execute(c, dto.GetStockInput{SKU: "A"})
	if a.OnHand != 3 || a.Reserved != 0 {
		t.Fatalf("after redelivery A=%+v, want on_hand 3 reserved 0", a)
	}
}

// When the released quantity was taken by someone else in the meantime the
// sale cannot be covered: COMMIT_SHORT, a visible terminal state, with the
// stock untouched (never negative) and no silent drop.
func TestCommitStock_AfterExpire_Short(t *testing.T) {
	c := context.Background()
	f := newFixture()
	if _, err := f.setStock.Execute(c, dto.SetStockInput{SKU: "A", OnHand: 2}); err != nil {
		t.Fatal(err)
	}
	reserveAt(t, f, f.clock.t.Add(-25*time.Hour), "o-1", 2)
	sweep(t, f)
	reserveAt(t, f, f.clock.t, "o-2", 2) // takes what the sweep gave back
	f.rec.events = nil

	out, err := f.commit.Execute(c, dto.CommitStockInput{OrderID: "o-1"})
	if err != nil || out.Status != "COMMIT_SHORT" {
		t.Fatalf("commit after expire = %+v, %v; want COMMIT_SHORT", out, err)
	}
	a, _ := f.getStock.Execute(c, dto.GetStockInput{SKU: "A"})
	if a.OnHand != 2 || a.Reserved != 2 {
		t.Fatalf("short must not move stock: A=%+v", a)
	}
	if len(f.rec.events) != 0 {
		t.Fatalf("events = %d, want none", len(f.rec.events))
	}

	// Redelivery is final and changes nothing; the other order still sells.
	if _, err := f.commit.Execute(c, dto.CommitStockInput{OrderID: "o-1"}); !errors.Is(err, reservation.ErrNotReserved) {
		t.Fatalf("redelivery: %v", err)
	}
	if _, err := f.commit.Execute(c, dto.CommitStockInput{OrderID: "o-2"}); err != nil {
		t.Fatalf("o-2 commit: %v", err)
	}
	a, _ = f.getStock.Execute(c, dto.GetStockInput{SKU: "A"})
	if a.OnHand != 0 || a.Reserved != 0 {
		t.Fatalf("after o-2 A=%+v, want on_hand 0 reserved 0", a)
	}
}

// onFirstLoad runs hook once, right before the first stock load: after a use
// case read its reservation, before it reads the stock.
type onFirstLoad struct {
	stock.Repository
	hook func()
}

func (i *onFirstLoad) FindBySKU(c context.Context, sku stock.SKU) (*stock.Stock, error) {
	if h := i.hook; h != nil {
		i.hook = nil
		h()
	}

	return i.Repository.FindBySKU(c, sku)
}

// A reservation that moved between being read and the stock being loaded makes
// the aggregate fail on a stock that no longer matches (here: nothing reserved
// any more). That is a lost race, retryable, not a final domain error.
func TestReservationMovedWhileHandled_IsStale(t *testing.T) {
	c := context.Background()

	t.Run("release over an expired reservation", func(t *testing.T) {
		f := newFixture()
		if _, err := f.setStock.Execute(c, dto.SetStockInput{SKU: "A", OnHand: 5}); err != nil {
			t.Fatal(err)
		}
		reserveAt(t, f, f.clock.t.Add(-25*time.Hour), "o-1", 2)
		hooked := &onFirstLoad{Repository: f.stocks, hook: func() { sweep(t, f) }}
		release := usecase.NewReleaseStock(f.reservations, hooked, f.rec, f.clock)

		if _, err := release.Execute(c, dto.ReleaseStockInput{OrderID: "o-1"}); !errors.Is(err, reservation.ErrStale) {
			t.Fatalf("release = %v, want ErrStale", err)
		}
	})

	t.Run("expire over a committed reservation", func(t *testing.T) {
		f := newFixture()
		if _, err := f.setStock.Execute(c, dto.SetStockInput{SKU: "A", OnHand: 5}); err != nil {
			t.Fatal(err)
		}
		reserveAt(t, f, f.clock.t.Add(-25*time.Hour), "o-1", 2)
		hooked := &onFirstLoad{Repository: f.stocks, hook: func() {
			if _, err := f.commit.Execute(c, dto.CommitStockInput{OrderID: "o-1"}); err != nil {
				t.Fatal(err)
			}
		}}
		expire := usecase.NewExpireReservations(f.reservations, hooked, f.rec, f.clock)

		if _, err := expire.Execute(c, dto.ExpireReservationsInput{OlderThan: 24 * time.Hour}); !errors.Is(err, reservation.ErrStale) {
			t.Fatalf("expire = %v, want ErrStale", err)
		}
	})
}

var errLookup = errors.New("lookup fault")

// failNthFind fails the n-th FindByOrderID (1-based) with errLookup.
type failNthFind struct {
	reservation.Repository
	n, calls int
}

func (i *failNthFind) FindByOrderID(c context.Context, orderID string) (*reservation.Reservation, error) {
	i.calls++
	if i.calls == i.n {
		return nil, errLookup
	}

	return i.Repository.FindByOrderID(c, orderID)
}

// When the reservation moved under CommitStock and the re-read that classifies
// the failure itself fails, the original domain error (final: the consumer
// drops the message for good) must not leak out: the lookup error is returned,
// which is infrastructure and retried.
func TestCommitStock_RereadFailureIsNotFinal(t *testing.T) {
	c := context.Background()
	f := newFixture()
	if _, err := f.setStock.Execute(c, dto.SetStockInput{SKU: "A", OnHand: 5}); err != nil {
		t.Fatal(err)
	}
	reserveAt(t, f, f.clock.t.Add(-25*time.Hour), "o-1", 2)
	flaky := &failNthFind{Repository: f.reservations, n: 2} // 1st: the use case's read, 2nd: the re-read
	hooked := &onFirstLoad{Repository: f.stocks, hook: func() { sweep(t, f) }}
	commit := usecase.NewCommitStock(flaky, hooked, f.rec, f.clock)

	_, err := commit.Execute(c, dto.CommitStockInput{OrderID: "o-1"})
	var de *kernel.Error
	if !errors.Is(err, errLookup) || errors.As(err, &de) {
		t.Fatalf("commit = %v, want the lookup error and no final kernel.Error", err)
	}
}
