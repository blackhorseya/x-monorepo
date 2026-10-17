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
	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
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
	clock        fixedClock
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
	f.clock = clock
	f.setStock = usecase.NewSetStock(f.stocks, clock)
	f.getStock = usecase.NewGetStock(f.stocks)
	f.getRes = usecase.NewGetReservation(f.reservations)
	f.reserve = usecase.NewReserveStock(f.reservations, f.stocks, f.rec, clock)
	f.release = usecase.NewReleaseStock(f.reservations, f.stocks, f.rec, clock)
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
	// An unknown order is not an error: OrderCancelled overtook OrderCreated (see TestRelease_BeforeReserve_Tombstone).
	if out, err := f.release.Execute(c, dto.ReleaseStockInput{OrderID: "o-cancelled-first"}); err != nil || out.Status != "CANCELLED" {
		t.Fatalf("release unknown = %+v, %v", out, err)
	}
	if _, err := f.getRes.Execute(c, dto.GetReservationInput{}); !errors.Is(err, reservation.ErrInvalidOrderID) {
		t.Fatalf("get empty id: %v", err)
	}
	if _, err := f.getRes.Execute(c, dto.GetReservationInput{OrderID: "unknown"}); !errors.Is(err, reservation.ErrNotFound) {
		t.Fatalf("get unknown: %v", err)
	}
}

// OrderCancelled can overtake OrderCreated (topics carry no ordering): the
// release writes a CANCELLED tombstone and the late create reserves nothing.
func TestRelease_BeforeReserve_Tombstone(t *testing.T) {
	c := context.Background()
	f := newFixture()
	if _, err := f.setStock.Execute(c, dto.SetStockInput{SKU: "A", OnHand: 5}); err != nil {
		t.Fatal(err)
	}

	rel, err := f.release.Execute(c, dto.ReleaseStockInput{OrderID: "o-late"})
	if err != nil || rel.Status != "CANCELLED" || len(rel.Lines) != 0 {
		t.Fatalf("release before reserve = %+v, %v", rel, err)
	}
	if len(f.rec.events) != 0 {
		t.Fatalf("tombstone published %d events, want 0", len(f.rec.events))
	}

	res, err := f.reserve.Execute(c, dto.ReserveStockInput{OrderID: "o-late", Lines: []dto.Line{{SKU: "A", Quantity: 2}}})
	if err != nil || res.Status != "CANCELLED" {
		t.Fatalf("late reserve = %+v, %v", res, err)
	}
	a, _ := f.getStock.Execute(c, dto.GetStockInput{SKU: "A"})
	if a.Reserved != 0 || len(f.rec.events) != 0 {
		t.Fatalf("late reserve touched state: reserved=%d events=%d", a.Reserved, len(f.rec.events))
	}
	// A redelivered OrderCancelled is a final no, not a second tombstone.
	if _, err := f.release.Execute(c, dto.ReleaseStockInput{OrderID: "o-late"}); !errors.Is(err, reservation.ErrNotReserved) {
		t.Fatalf("release tombstone: %v", err)
	}
}

// racing wraps a repository so the first Add loses to a competing writer:
// compete runs against the real repo (as the other consumer's committed use
// case would), then the caller gets ErrDuplicate.
type racing struct {
	reservation.Repository
	compete func(c context.Context) error
	raced   bool
}

func (r *racing) Add(c context.Context, x *reservation.Reservation) error {
	if !r.raced {
		r.raced = true
		if err := r.compete(c); err != nil {
			return err
		}
		return reservation.ErrDuplicate
	}
	return r.Repository.Add(c, x)
}

// Losing the insert race must be retried, never dropped: a dropped
// OrderCancelled would leave the competing reservation RESERVED forever.
func TestAddRace_IsRetried(t *testing.T) {
	c := context.Background()
	reserveIn := dto.ReserveStockInput{OrderID: "o-race", Lines: []dto.Line{{SKU: "A", Quantity: 1}}}
	releaseIn := dto.ReleaseStockInput{OrderID: "o-race"}

	for _, tc := range []struct {
		name string
		// loser runs through the racing repo; winner is what commits first.
		loser, winner func(f *fixture, repo reservation.Repository) error
		want          string
	}{
		{"release loses to reserve",
			func(f *fixture, repo reservation.Repository) error {
				_, err := usecase.NewReleaseStock(repo, f.stocks, f.rec, f.clock).Execute(c, releaseIn)
				return err
			},
			func(f *fixture, _ reservation.Repository) error {
				_, err := f.reserve.Execute(c, reserveIn)
				return err
			},
			"RELEASED"},
		{"reserve loses to tombstone",
			func(f *fixture, repo reservation.Repository) error {
				_, err := usecase.NewReserveStock(repo, f.stocks, f.rec, f.clock).Execute(c, reserveIn)
				return err
			},
			func(f *fixture, _ reservation.Repository) error {
				_, err := f.release.Execute(c, releaseIn)
				return err
			},
			"CANCELLED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			if _, err := f.setStock.Execute(c, dto.SetStockInput{SKU: "A", OnHand: 5}); err != nil {
				t.Fatal(err)
			}
			repo := &racing{Repository: f.reservations, compete: func(context.Context) error { return tc.winner(f, nil) }}

			err := tc.loser(f, repo)
			var de *kernel.Error
			if !errors.Is(err, kernel.ErrConcurrentUpdate) || errors.As(err, &de) {
				t.Fatalf("lost race err = %v, want kernel.ErrConcurrentUpdate and not a kernel.Error", err)
			}
			// The retry finds the winner's row and settles on it.
			if err := tc.loser(f, repo); err != nil {
				t.Fatalf("retry: %v", err)
			}
			got, _ := f.getRes.Execute(c, dto.GetReservationInput{OrderID: "o-race"})
			if got.Status != tc.want {
				t.Fatalf("after retry status = %s, want %s", got.Status, tc.want)
			}
			// Stock is only asserted where this test can see the truth: the memory
			// Transactor does not roll back, so a losing ReserveStock's stock write
			// stays here; under Postgres the transaction discards it (provider_test).
			if a, _ := f.getStock.Execute(c, dto.GetStockInput{SKU: "A"}); tc.want == "RELEASED" && a.Reserved != 0 {
				t.Fatalf("after retry reserved = %d, want 0", a.Reserved)
			}
		})
	}
}
