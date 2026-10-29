package usecase_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
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
