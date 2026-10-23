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
	"github.com/blackhorseya/x-monorepo/pkgs/platform/pagination"
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
	listStocks   *usecase.ListStocks
	getRes       *usecase.GetReservation
	listRes      *usecase.ListReservations
	reserve      *usecase.ReserveStock
	release      *usecase.ReleaseStock
}

func newFixture() *fixture {
	f := &fixture{stocks: memory.NewStockRepository(), reservations: memory.NewReservationRepository(), rec: &recorder{}}
	clock := fixedClock{t: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)}
	f.clock = clock
	f.setStock = usecase.NewSetStock(f.stocks, clock)
	f.getStock = usecase.NewGetStock(f.stocks)
	f.listStocks = usecase.NewListStocks(f.stocks)
	f.getRes = usecase.NewGetReservation(f.reservations)
	f.listRes = usecase.NewListReservations(f.reservations)
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

// ListStocks pages ascending by SKU; the token resumes after the last SKU
// and a tampered token is rejected.
func TestListStocks(t *testing.T) {
	c := context.Background()
	f := newFixture()
	for _, sku := range []string{"C", "A", "B"} {
		if _, err := f.setStock.Execute(c, dto.SetStockInput{SKU: sku, OnHand: 1}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := f.listStocks.Execute(c, dto.ListStocksInput{PageSize: 2})
	if err != nil || len(page.Stocks) != 2 || page.Stocks[0].SKU != "A" || page.Stocks[1].SKU != "B" || page.NextPageToken == "" {
		t.Fatalf("page 1 = %+v, %v", page, err)
	}
	last, err := f.listStocks.Execute(c, dto.ListStocksInput{PageSize: 2, PageToken: page.NextPageToken})
	if err != nil || len(last.Stocks) != 1 || last.Stocks[0].SKU != "C" || last.NextPageToken != "" {
		t.Fatalf("page 2 = %+v, %v", last, err)
	}
	all, err := f.listStocks.Execute(c, dto.ListStocksInput{})
	if err != nil || len(all.Stocks) != 3 || all.NextPageToken != "" {
		t.Fatalf("default page = %+v, %v", all, err)
	}
	if _, err := f.listStocks.Execute(c, dto.ListStocksInput{PageToken: "not-a-token"}); !errors.Is(err, pagination.ErrInvalidToken) {
		t.Fatalf("bad token: %v", err)
	}
	if _, err := f.listStocks.Execute(c, dto.ListStocksInput{PageSize: -1}); err == nil {
		t.Fatal("negative page size must fail")
	}

	// Filters: prefix and only-reserved narrow the listing, and a token minted
	// under one filter does not continue under another.
	for _, sku := range []string{"AB", "AC"} {
		if _, err := f.setStock.Execute(c, dto.SetStockInput{SKU: sku, OnHand: 3}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.reserve.Execute(c, dto.ReserveStockInput{OrderID: "o-list", Lines: []dto.Line{{SKU: "AC", Quantity: 1}}}); err != nil {
		t.Fatal(err)
	}
	byPrefix, err := f.listStocks.Execute(c, dto.ListStocksInput{SKUPrefix: "A", PageSize: 2})
	if err != nil || len(byPrefix.Stocks) != 2 || byPrefix.Stocks[0].SKU != "A" || byPrefix.Stocks[1].SKU != "AB" || byPrefix.NextPageToken == "" {
		t.Fatalf("prefix page 1 = %+v, %v", byPrefix, err)
	}
	rest, err := f.listStocks.Execute(c, dto.ListStocksInput{SKUPrefix: "A", PageSize: 2, PageToken: byPrefix.NextPageToken})
	if err != nil || len(rest.Stocks) != 1 || rest.Stocks[0].SKU != "AC" || rest.NextPageToken != "" {
		t.Fatalf("prefix page 2 = %+v, %v", rest, err)
	}
	if _, err := f.listStocks.Execute(c, dto.ListStocksInput{PageSize: 2, PageToken: byPrefix.NextPageToken}); !errors.Is(err, pagination.ErrInvalidToken) {
		t.Fatalf("token under another filter: %v", err)
	}
	reserved, err := f.listStocks.Execute(c, dto.ListStocksInput{OnlyReserved: true})
	if err != nil || len(reserved.Stocks) != 1 || reserved.Stocks[0].SKU != "AC" || reserved.Stocks[0].Reserved != 1 {
		t.Fatalf("only reserved = %+v, %v", reserved, err)
	}
	if none, err := f.listStocks.Execute(c, dto.ListStocksInput{SKUPrefix: "Z", OnlyReserved: true}); err != nil || len(none.Stocks) != 0 {
		t.Fatalf("no match = %+v, %v", none, err)
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

// ListReservations is newest first (the fixed clock makes every row the same
// age, so the order id breaks the tie), filters by status, and its token is
// bound to the filter it was issued for.
func TestListReservations(t *testing.T) {
	c := context.Background()
	f := newFixture()
	if _, err := f.setStock.Execute(c, dto.SetStockInput{SKU: "A", OnHand: 10}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"o-1", "o-3"} {
		if _, err := f.reserve.Execute(c, dto.ReserveStockInput{OrderID: id, Lines: []dto.Line{{SKU: "A", Quantity: 1}}}); err != nil {
			t.Fatal(err)
		}
	}
	// o-2 cancelled before it was created: a CANCELLED tombstone.
	if _, err := f.release.Execute(c, dto.ReleaseStockInput{OrderID: "o-2"}); err != nil {
		t.Fatal(err)
	}

	page, err := f.listRes.Execute(c, dto.ListReservationsInput{PageSize: 2})
	if err != nil || len(page.Reservations) != 2 || page.Reservations[0].OrderID != "o-3" || page.Reservations[1].OrderID != "o-2" || page.NextPageToken == "" {
		t.Fatalf("page 1 = %+v, %v", page, err)
	}
	last, err := f.listRes.Execute(c, dto.ListReservationsInput{PageSize: 2, PageToken: page.NextPageToken})
	if err != nil || len(last.Reservations) != 1 || last.Reservations[0].OrderID != "o-1" || last.NextPageToken != "" {
		t.Fatalf("page 2 = %+v, %v", last, err)
	}
	reserved, err := f.listRes.Execute(c, dto.ListReservationsInput{Status: "RESERVED", PageSize: 1})
	if err != nil || len(reserved.Reservations) != 1 || reserved.Reservations[0].OrderID != "o-3" || reserved.NextPageToken == "" {
		t.Fatalf("RESERVED page 1 = %+v, %v", reserved, err)
	}
	// The token carries its filter: replayed with another one it is rejected.
	if _, err := f.listRes.Execute(c, dto.ListReservationsInput{Status: "CANCELLED", PageToken: reserved.NextPageToken}); !errors.Is(err, pagination.ErrInvalidToken) {
		t.Fatalf("token with another filter: %v", err)
	}
	if _, err := f.listRes.Execute(c, dto.ListReservationsInput{Status: "BOGUS"}); !errors.Is(err, reservation.ErrInvalidStatusFilter) {
		t.Fatalf("bogus status: %v", err)
	}
	if _, err := f.listRes.Execute(c, dto.ListReservationsInput{PageToken: "not-a-token"}); !errors.Is(err, pagination.ErrInvalidToken) {
		t.Fatalf("bad token: %v", err)
	}
	if _, err := f.listRes.Execute(c, dto.ListReservationsInput{PageSize: -1}); err == nil {
		t.Fatal("negative page size must fail")
	}
}
