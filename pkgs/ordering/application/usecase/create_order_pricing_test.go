package usecase_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/ownership"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/memory"
)

type pricingFixture struct {
	create *usecase.CreateOrder
	get    *usecase.GetOrder
	list   *usecase.ListOrders
	prices *memory.PriceList
	events *lockedRecorder
	repo   order.Repository
}

func newPricingFixture(repo order.Repository, prices *memory.PriceList) pricingFixture {
	events := &lockedRecorder{}
	clock := fixedClock{t: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)}
	own := ownership.New(adminRole)

	return pricingFixture{
		create: usecase.NewCreateOrder(repo, events, atomicIDs{}, clock, own, prices),
		get:    usecase.NewGetOrder(repo, own),
		list:   usecase.NewListOrders(repo, own),
		prices: prices,
		events: events,
		repo:   repo,
	}
}

func lines(pairs ...any) []dto.OrderLine {
	var out []dto.OrderLine
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, dto.OrderLine{SKU: pairs[i].(string), Quantity: int32(pairs[i+1].(int))})
	}

	return out
}

// The caller says what and how many; what it costs is the catalog's answer at the
// moment the order is placed, written on the lines as a snapshot.
func TestCreateOrder_PriceComesFromTheCatalog(t *testing.T) {
	repos(t, func(t *testing.T, repo order.Repository) {
		f := newPricingFixture(repo, memory.NewPriceList().Set("A", 500, "USD").Set("B", 250, "USD"))

		out, err := f.create.Execute(as(alice), dto.CreateOrderInput{Items: lines("A", 2, "B", 1)})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Items) != 2 || out.Items[0].UnitPrice != (dto.Money{AmountMinor: 500, Currency: "USD"}) || out.Items[1].UnitPrice != (dto.Money{AmountMinor: 250, Currency: "USD"}) {
			t.Fatalf("lines = %+v, want the catalog prices as the snapshot", out.Items)
		}
		if out.Total != (dto.Money{AmountMinor: 1250, Currency: "USD"}) {
			t.Fatalf("total = %+v, want 1250 USD", out.Total)
		}
		if calls := f.prices.Calls(); calls != 1 {
			t.Fatalf("the catalog was asked %d times, want once for the whole order", calls)
		}
		if got := f.prices.LastSKUs(); len(got) != 2 || got[0] != "A" || got[1] != "B" {
			t.Fatalf("asked for %v, want [A B]", got)
		}
	})
}

func TestCreateOrder_RepeatedSKUIsQuotedOnce(t *testing.T) {
	repos(t, func(t *testing.T, repo order.Repository) {
		f := newPricingFixture(repo, memory.NewPriceList().Set("A", 500, "USD"))

		out, err := f.create.Execute(as(alice), dto.CreateOrderInput{Items: lines("A", 1, "A", 2)})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Items) != 2 || out.Total.AmountMinor != 1500 {
			t.Fatalf("order = %+v, want two lines and 1500", out)
		}
		if got := f.prices.LastSKUs(); len(got) != 1 {
			t.Fatalf("asked for %v, want A once", got)
		}
	})
}

// What was sold at 5.00 stays sold at 5.00: the snapshot is the order's own copy.
func TestCreateOrder_RepriceDoesNotTouchPlacedOrders(t *testing.T) {
	repos(t, func(t *testing.T, repo order.Repository) {
		f := newPricingFixture(repo, memory.NewPriceList().Set("A", 500, "USD"))

		first, err := f.create.Execute(as(alice), dto.CreateOrderInput{Items: lines("A", 2)})
		if err != nil {
			t.Fatal(err)
		}
		f.prices.Set("A", 900, "EUR") // reprice, and even change the currency

		again, err := f.get.Execute(as(alice), dto.GetOrderInput{OrderID: first.ID})
		if err != nil {
			t.Fatal(err)
		}
		if again.Items[0].UnitPrice != (dto.Money{AmountMinor: 500, Currency: "USD"}) || again.Total != (dto.Money{AmountMinor: 1000, Currency: "USD"}) {
			t.Fatalf("placed order after reprice = %+v, want the original 500 USD snapshot", again)
		}

		next, err := f.create.Execute(as(alice), dto.CreateOrderInput{Items: lines("A", 1)})
		if err != nil || next.Items[0].UnitPrice != (dto.Money{AmountMinor: 900, Currency: "EUR"}) {
			t.Fatalf("new order = %+v, %v; want the new catalog price", next, err)
		}
	})
}

func TestCreateOrder_RefusesWhatTheCatalogCannotQuote(t *testing.T) {
	repos(t, func(t *testing.T, repo order.Repository) {
		prices := memory.NewPriceList().Set("A", 500, "USD").Set("OLD", 100, "USD")
		prices.Deactivate("OLD")
		f := newPricingFixture(repo, prices)
		before := f.count(t)

		for name, tc := range map[string]struct {
			items []dto.OrderLine
			want  error
		}{
			"unknown":           {lines("NOPE", 1), order.ErrUnknownSKU},
			"deactivated":       {lines("OLD", 1), order.ErrInactiveSKU},
			"one bad of many":   {lines("A", 1, "NOPE", 1), order.ErrUnknownSKU},
			"inactive of many":  {lines("A", 1, "OLD", 1), order.ErrInactiveSKU},
			"unknown first":     {lines("OLD", 1, "NOPE", 1), order.ErrUnknownSKU},
			"empty sku":         {lines("", 1), order.ErrInvalidSKU},
			"zero quantity":     {lines("A", 0), order.ErrInvalidQuantity},
			"negative quantity": {lines("A", -1), order.ErrInvalidQuantity},
		} {
			if _, err := f.create.Execute(as(alice), dto.CreateOrderInput{Items: tc.items}); !errors.Is(err, tc.want) {
				t.Errorf("%s: err = %v, want %v", name, err, tc.want)
			}
		}
		if after := f.count(t); after != before {
			t.Fatalf("%d orders stored after the refusals, want %d", after, before)
		}
		if n := f.events.created(); n != 0 {
			t.Fatalf("%d OrderCreated published for refused orders", n)
		}
	})
}

func (f pricingFixture) count(t *testing.T) int {
	t.Helper()
	out, err := f.list.Execute(as(ops), dto.ListOrdersInput{})
	if err != nil {
		t.Fatal(err)
	}

	return len(out.Orders)
}

// Each product has one currency, an order has one: the second currency is refused
// before anything is written.
func TestCreateOrder_RefusesMixedCurrencies(t *testing.T) {
	repos(t, func(t *testing.T, repo order.Repository) {
		f := newPricingFixture(repo, memory.NewPriceList().Set("USD-1", 500, "USD").Set("USD-2", 100, "USD").Set("EUR-1", 400, "EUR"))
		before := f.count(t)

		if _, err := f.create.Execute(as(alice), dto.CreateOrderInput{Items: lines("USD-1", 1, "EUR-1", 1)}); !errors.Is(err, order.ErrMixedCurrency) {
			t.Fatalf("mixed order err = %v, want ErrMixedCurrency", err)
		}
		if f.count(t) != before || f.events.created() != 0 {
			t.Fatal("a refused mixed-currency order left something behind")
		}
		// one currency at a time is fine, either one
		for _, items := range [][]dto.OrderLine{lines("USD-1", 1, "USD-2", 3), lines("EUR-1", 2)} {
			if _, err := f.create.Execute(as(alice), dto.CreateOrderInput{Items: items}); err != nil {
				t.Fatalf("single-currency order %v: %v", items, err)
			}
		}
	})
}

func TestCreateOrder_CatalogFailureIsNotAnOrder(t *testing.T) {
	repos(t, func(t *testing.T, repo order.Repository) {
		boom := errors.New("catalog unreachable")
		f := newPricingFixture(repo, memory.NewPriceList().Set("A", 500, "USD").FailWith(boom))
		before := f.count(t)

		_, err := f.create.Execute(as(alice), dto.CreateOrderInput{Items: lines("A", 1)})
		var ke *kernel.Error
		if !errors.Is(err, boom) || errors.As(err, &ke) {
			t.Fatalf("err = %v, want the infrastructure error (not a domain error: it must retry, not read as a bad request)", err)
		}
		if f.count(t) != before || f.events.created() != 0 {
			t.Fatal("an order was written without a price")
		}
	})
}

// The retry of a request that was already served is answered from the order, not from the
// catalog: the product may since have been repriced or taken off sale.
func TestCreateOrder_ReplayDoesNotAskTheCatalogAgain(t *testing.T) {
	repos(t, func(t *testing.T, repo order.Repository) {
		f := newPricingFixture(repo, memory.NewPriceList().Set("A", 500, "USD").Set("B", 250, "USD"))
		in := dto.CreateOrderInput{Items: lines("A", 1, "B", 2), IdempotencyKey: "k-pricing"}

		first, err := f.create.Execute(as(alice), in)
		if err != nil {
			t.Fatal(err)
		}
		asked := f.prices.Calls()

		f.prices.Set("A", 999, "USD")
		f.prices.Deactivate("B")
		again, err := f.create.Execute(as(alice), in)
		if err != nil || again.ID != first.ID || !again.Replayed || again.Total != first.Total {
			t.Fatalf("replay = %+v, %v; want the original order %s", again, err, first.ID)
		}
		if f.prices.Calls() != asked {
			t.Fatalf("the replay asked the catalog (%d -> %d calls)", asked, f.prices.Calls())
		}
		if f.events.created() != 1 {
			t.Fatalf("%d OrderCreated, want 1", f.events.created())
		}
	})
}

// A refusal spends nothing: the key stays free, and the same request goes through once the
// catalog can price it.
func TestCreateOrder_RefusedRequestDoesNotSpendTheKey(t *testing.T) {
	repos(t, func(t *testing.T, repo order.Repository) {
		f := newPricingFixture(repo, memory.NewPriceList())
		in := dto.CreateOrderInput{Items: lines("LATE", 1), IdempotencyKey: "k-late"}

		if _, err := f.create.Execute(as(alice), in); !errors.Is(err, order.ErrUnknownSKU) {
			t.Fatalf("before the product exists: %v", err)
		}
		f.prices.Set("LATE", 700, "USD")
		out, err := f.create.Execute(as(alice), in)
		if err != nil || out.Replayed || out.Total.AmountMinor != 700 {
			t.Fatalf("after the product exists = %+v, %v; want a new order", out, err)
		}
	})
}

// v2: the key is spent on sku and quantity only.
func TestCreateOrder_KeyedRequestsCompareBySKUAndQuantity(t *testing.T) {
	repos(t, func(t *testing.T, repo order.Repository) {
		f := newPricingFixture(repo, memory.NewPriceList().Set("A", 500, "USD").Set("B", 250, "USD"))
		if _, err := f.create.Execute(as(alice), dto.CreateOrderInput{Items: lines("A", 1, "B", 2), IdempotencyKey: "k-v2"}); err != nil {
			t.Fatal(err)
		}
		for name, items := range map[string][]dto.OrderLine{
			"quantity":  lines("A", 1, "B", 3),
			"reordered": lines("B", 2, "A", 1),
			"sku":       lines("A", 1, "A", 2),
			"fewer":     lines("A", 1),
		} {
			if _, err := f.create.Execute(as(alice), dto.CreateOrderInput{Items: items, IdempotencyKey: "k-v2"}); !errors.Is(err, order.ErrIdempotencyKeyReused) {
				t.Errorf("%s: err = %v, want ErrIdempotencyKeyReused", name, err)
			}
		}
	})
}

// Orders stored before the price came from the catalog carry the v1 fingerprint (it covered
// the price the caller typed). The same retry must still replay them — and nothing else may.
func TestCreateOrder_LegacyV1KeysStillReplay(t *testing.T) {
	repos(t, func(t *testing.T, repo order.Repository) {
		f := newPricingFixture(repo, memory.NewPriceList().Set("A", 777, "USD").Set("B", 888, "USD"))
		c := context.Background()
		usd := func(amount int64) kernel.Money {
			m, err := kernel.NewMoney(amount, "USD")
			if err != nil {
				t.Fatal(err)
			}

			return m
		}
		a, _ := order.NewLineItem("A", 1, usd(500))
		b, _ := order.NewLineItem("B", 2, usd(250))
		now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
		legacy, err := order.Reconstitute(order.ReconstituteParams{
			ID: "legacy-" + t.Name(), CustomerID: "acct-alice", Items: []order.LineItem{a, b}, Status: order.StatusConfirmed,
			CreatedAt: now, UpdatedAt: now, IdempotencyKey: "k-legacy", RequestHash: order.Fingerprint([]order.LineItem{a, b}),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.Save(c, legacy); err != nil {
			t.Fatal(err)
		}
		before := f.count(t)

		out, err := f.create.Execute(as(alice), dto.CreateOrderInput{Items: lines("A", 1, "B", 2), IdempotencyKey: "k-legacy"})
		if err != nil || out.ID != legacy.ID().String() || !out.Replayed {
			t.Fatalf("retry of a v1 order = %+v, %v; want a replay of %s", out, err, legacy.ID())
		}
		if out.Total.AmountMinor != 1000 || out.Items[0].UnitPrice.AmountMinor != 500 {
			t.Fatalf("replayed %+v, want the order's own snapshot, not today's catalog price", out)
		}
		if f.prices.Calls() != 0 || f.count(t) != before || f.events.created() != 0 {
			t.Fatal("the replay of a v1 order asked the catalog or wrote something")
		}

		for name, items := range map[string][]dto.OrderLine{
			"quantity":  lines("A", 1, "B", 3),
			"reordered": lines("B", 2, "A", 1),
			"sku":       lines("A", 1, "C", 2),
		} {
			if _, err := f.create.Execute(as(alice), dto.CreateOrderInput{Items: items, IdempotencyKey: "k-legacy"}); !errors.Is(err, order.ErrIdempotencyKeyReused) {
				t.Errorf("%s: err = %v, want ErrIdempotencyKeyReused", name, err)
			}
		}
	})
}
