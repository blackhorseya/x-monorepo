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
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/memory"
)

type fixedClock struct{ t time.Time }

func (i fixedClock) Now() time.Time { return i.t }

// recorder is a port.EventPublisher that keeps what was published, in order.
type recorder struct{ events []reservation.Event }

func (r *recorder) Publish(_ context.Context, evs ...reservation.Event) error {
	r.events = append(r.events, evs...)
	return nil
}

type fixture struct {
	stocks       *memory.StockRepository
	reservations *memory.ReservationRepository
	rec          *recorder
	setStock     *usecase.SetStock
	getStock     *usecase.GetStock
	getRes       *usecase.GetReservation
	reserve      *usecase.ReserveStock
	release      *usecase.ReleaseStock
}

func newFixture() *fixture {
	f := &fixture{stocks: memory.NewStockRepository(), reservations: memory.NewReservationRepository(), rec: &recorder{}}
	clock := fixedClock{t: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)}
	tx := memory.NewTransactor()
	f.setStock = usecase.NewSetStock(f.stocks, tx, clock)
	f.getStock = usecase.NewGetStock(f.stocks)
	f.getRes = usecase.NewGetReservation(f.reservations)
	f.reserve = usecase.NewReserveStock(f.reservations, f.stocks, tx, f.rec, clock)
	f.release = usecase.NewReleaseStock(f.reservations, f.stocks, tx, f.rec, clock)
	return f
}

func TestSetGetStock(t *testing.T) {
	c := context.Background()
	f := newFixture()

	out, err := f.setStock.Execute(c, dto.SetStockInput{SKU: "A", OnHand: 5})
	if err != nil || out.OnHand != 5 || out.Available != 5 {
		t.Fatalf("set (create) = %+v, %v", out, err)
	}
	out, err = f.setStock.Execute(c, dto.SetStockInput{SKU: "A", OnHand: 7})
	if err != nil || out.OnHand != 7 {
		t.Fatalf("set (restock) = %+v, %v", out, err)
	}
	if _, err := f.setStock.Execute(c, dto.SetStockInput{SKU: "", OnHand: 1}); !errors.Is(err, stock.ErrInvalidSKU) {
		t.Fatalf("empty sku: %v", err)
	}
	if _, err := f.setStock.Execute(c, dto.SetStockInput{SKU: "A", OnHand: -1}); !errors.Is(err, stock.ErrNegativeQuantity) {
		t.Fatalf("negative: %v", err)
	}

	got, err := f.getStock.Execute(c, dto.GetStockInput{SKU: "A"})
	if err != nil || got.SKU != "A" || got.OnHand != 7 || got.Reserved != 0 {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if _, err := f.getStock.Execute(c, dto.GetStockInput{SKU: "nope"}); !errors.Is(err, stock.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := f.getStock.Execute(c, dto.GetStockInput{}); !errors.Is(err, stock.ErrInvalidSKU) {
		t.Fatalf("empty: %v", err)
	}
}

func TestReserveRelease(t *testing.T) {
	c := context.Background()
	f := newFixture()
	for sku, n := range map[string]int32{"A": 5, "B": 1} {
		if _, err := f.setStock.Execute(c, dto.SetStockInput{SKU: sku, OnHand: n}); err != nil {
			t.Fatal(err)
		}
	}

	in := dto.ReserveStockInput{OrderID: "o-1", Lines: []dto.Line{{SKU: "A", Quantity: 2}, {SKU: "B", Quantity: 1}}}
	out, err := f.reserve.Execute(c, in)
	if err != nil || out.Status != "RESERVED" || len(out.Lines) != 2 || len(out.ShortfallSKUs) != 0 {
		t.Fatalf("reserve = %+v, %v", out, err)
	}
	a, _ := f.getStock.Execute(c, dto.GetStockInput{SKU: "A"})
	if a.Reserved != 2 || a.Available != 3 {
		t.Fatalf("A after reserve = %+v", a)
	}
	if len(f.rec.events) != 1 {
		t.Fatalf("events = %d, want Reserved", len(f.rec.events))
	}
	if _, ok := f.rec.events[0].(reservation.Reserved); !ok {
		t.Fatalf("event = %T", f.rec.events[0])
	}

	// Redelivery: same outcome, nothing reserved twice, no new event.
	again, err := f.reserve.Execute(c, in)
	if err != nil || again.Status != "RESERVED" {
		t.Fatalf("redelivery = %+v, %v", again, err)
	}
	a, _ = f.getStock.Execute(c, dto.GetStockInput{SKU: "A"})
	if a.Reserved != 2 || len(f.rec.events) != 1 {
		t.Fatalf("redelivery changed state: reserved=%d events=%d", a.Reserved, len(f.rec.events))
	}

	// Restocking below what is reserved is refused.
	if _, err := f.setStock.Execute(c, dto.SetStockInput{SKU: "A", OnHand: 1}); !errors.Is(err, stock.ErrBelowReserved) {
		t.Fatalf("restock below reserved: %v", err)
	}

	got, err := f.getRes.Execute(c, dto.GetReservationInput{OrderID: "o-1"})
	if err != nil || got.Status != "RESERVED" || got.Lines[1].SKU != "B" {
		t.Fatalf("get reservation = %+v, %v", got, err)
	}

	rel, err := f.release.Execute(c, dto.ReleaseStockInput{OrderID: "o-1"})
	if err != nil || rel.Status != "RELEASED" {
		t.Fatalf("release = %+v, %v", rel, err)
	}
	a, _ = f.getStock.Execute(c, dto.GetStockInput{SKU: "A"})
	b, _ := f.getStock.Execute(c, dto.GetStockInput{SKU: "B"})
	if a.Reserved != 0 || b.Reserved != 0 {
		t.Fatalf("after release A=%d B=%d", a.Reserved, b.Reserved)
	}
	if len(f.rec.events) != 2 {
		t.Fatalf("events = %d, want Reserved, Released", len(f.rec.events))
	}
	if _, ok := f.rec.events[1].(reservation.Released); !ok {
		t.Fatalf("event = %T", f.rec.events[1])
	}
	// Second OrderCancelled: final domain error, not a retry.
	if _, err := f.release.Execute(c, dto.ReleaseStockInput{OrderID: "o-1"}); !errors.Is(err, reservation.ErrNotReserved) {
		t.Fatalf("double release: %v", err)
	}
}

func TestReserve_Shortfall(t *testing.T) {
	c := context.Background()
	f := newFixture()
	if _, err := f.setStock.Execute(c, dto.SetStockInput{SKU: "A", OnHand: 1}); err != nil {
		t.Fatal(err)
	}

	// A short, C never stocked: rejected, nothing reserved, one Rejected event.
	out, err := f.reserve.Execute(c, dto.ReserveStockInput{OrderID: "o-2", Lines: []dto.Line{{SKU: "A", Quantity: 2}, {SKU: "C", Quantity: 1}}})
	if err != nil || out.Status != "REJECTED" || len(out.ShortfallSKUs) != 2 || out.ShortfallSKUs[0] != "A" || out.ShortfallSKUs[1] != "C" {
		t.Fatalf("reserve = %+v, %v", out, err)
	}
	a, _ := f.getStock.Execute(c, dto.GetStockInput{SKU: "A"})
	if a.Reserved != 0 {
		t.Fatalf("rejected reservation touched stock: %+v", a)
	}
	ev, ok := f.rec.events[0].(reservation.Rejected)
	if len(f.rec.events) != 1 || !ok || ev.OrderID() != "o-2" {
		t.Fatalf("events = %#v", f.rec.events)
	}

	// Persisted, so redelivery is a no-op and the outcome is readable.
	got, err := f.getRes.Execute(c, dto.GetReservationInput{OrderID: "o-2"})
	if err != nil || got.Status != "REJECTED" || len(got.ShortfallSKUs) != 2 {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if _, err := f.reserve.Execute(c, dto.ReserveStockInput{OrderID: "o-2", Lines: []dto.Line{{SKU: "A", Quantity: 2}}}); err != nil || len(f.rec.events) != 1 {
		t.Fatalf("redelivery: err=%v events=%d", err, len(f.rec.events))
	}
	// Releasing a rejected reservation is final too.
	if _, err := f.release.Execute(c, dto.ReleaseStockInput{OrderID: "o-2"}); !errors.Is(err, reservation.ErrNotReserved) {
		t.Fatalf("release rejected: %v", err)
	}
}

func TestValidation(t *testing.T) {
	c := context.Background()
	f := newFixture()

	if _, err := f.reserve.Execute(c, dto.ReserveStockInput{Lines: []dto.Line{{SKU: "A", Quantity: 1}}}); !errors.Is(err, reservation.ErrInvalidOrderID) {
		t.Fatalf("empty order id: %v", err)
	}
	if _, err := f.reserve.Execute(c, dto.ReserveStockInput{OrderID: "o", Lines: []dto.Line{{SKU: "A", Quantity: 0}}}); !errors.Is(err, reservation.ErrInvalidQuantity) {
		t.Fatalf("zero qty: %v", err)
	}
	if _, err := f.reserve.Execute(c, dto.ReserveStockInput{OrderID: "o"}); !errors.Is(err, reservation.ErrNoLines) {
		t.Fatalf("no lines: %v", err)
	}
	if _, err := f.release.Execute(c, dto.ReleaseStockInput{}); !errors.Is(err, reservation.ErrInvalidOrderID) {
		t.Fatalf("release empty id: %v", err)
	}
	if _, err := f.release.Execute(c, dto.ReleaseStockInput{OrderID: "unknown"}); !errors.Is(err, reservation.ErrNotFound) {
		t.Fatalf("release unknown: %v", err)
	}
	if _, err := f.getRes.Execute(c, dto.GetReservationInput{}); !errors.Is(err, reservation.ErrInvalidOrderID) {
		t.Fatalf("get empty id: %v", err)
	}
	if _, err := f.getRes.Execute(c, dto.GetReservationInput{OrderID: "unknown"}); !errors.Is(err, reservation.ErrNotFound) {
		t.Fatalf("get unknown: %v", err)
	}
}
