package usecase_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/catalog/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/domain/product"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/pagination"
)

type fixedClock struct{ t *time.Time }

func (i fixedClock) Now() time.Time { return *i.t }

type fixture struct {
	now        time.Time
	products   *memory.ProductRepository
	upsert     *usecase.UpsertProduct
	get        *usecase.GetProduct
	list       *usecase.ListProducts
	deactivate *usecase.DeactivateProduct
	quote      *usecase.QuotePrices
}

func newFixture() *fixture {
	f := &fixture{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), products: memory.NewProductRepository()}
	clock := fixedClock{t: &f.now}
	f.upsert = usecase.NewUpsertProduct(f.products, clock)
	f.get = usecase.NewGetProduct(f.products)
	f.list = usecase.NewListProducts(f.products)
	f.deactivate = usecase.NewDeactivateProduct(f.products, clock)
	f.quote = usecase.NewQuotePrices(f.products)

	return f
}

func (f *fixture) put(t *testing.T, sku string, amount int64, currency string) {
	t.Helper()
	if _, err := f.upsert.Execute(context.Background(), dto.UpsertProductInput{SKU: sku, Name: "Name " + sku, Amount: amount, Currency: currency}); err != nil {
		t.Fatal(err)
	}
}

func TestUpsertProduct_CreateThenReprice(t *testing.T) {
	f := newFixture()
	c := context.Background()

	out, err := f.upsert.Execute(c, dto.UpsertProductInput{SKU: "A", Name: "Apple", Amount: 150, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	if out.SKU != "A" || out.Name != "Apple" || out.Price.Amount != 150 || out.Price.Currency != "USD" || !out.Active || !out.UpdateTime.Equal(f.now) {
		t.Fatalf("created = %+v", out)
	}

	f.now = f.now.Add(time.Hour)
	if _, err = f.upsert.Execute(c, dto.UpsertProductInput{SKU: "A", Name: "Green apple", Amount: 200, Currency: "EUR"}); err != nil {
		t.Fatal(err)
	}
	got, err := f.get.Execute(c, dto.GetProductInput{SKU: "A"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Green apple" || got.Price.Amount != 200 || got.Price.Currency != "EUR" || !got.UpdateTime.Equal(f.now) {
		t.Fatalf("Get after reprice = %+v, want the new name and price", got)
	}
}

func TestUpsertProduct_Reactivates(t *testing.T) {
	f := newFixture()
	c := context.Background()
	f.put(t, "A", 150, "USD")
	if _, err := f.deactivate.Execute(c, dto.DeactivateProductInput{SKU: "A"}); err != nil {
		t.Fatal(err)
	}

	out, err := f.upsert.Execute(c, dto.UpsertProductInput{SKU: "A", Name: "Apple", Amount: 150, Currency: "USD"})
	if err != nil || !out.Active {
		t.Fatalf("upsert of an inactive product = %+v, %v; want it active again", out, err)
	}
}

func TestUpsertProduct_Invalid(t *testing.T) {
	f := newFixture()
	c := context.Background()

	for name, tc := range map[string]struct {
		in   dto.UpsertProductInput
		want error
	}{
		"negative amount": {dto.UpsertProductInput{SKU: "A", Name: "Apple", Amount: -1, Currency: "USD"}, kernel.ErrInvalidAmount},
		"lower case code": {dto.UpsertProductInput{SKU: "A", Name: "Apple", Amount: 1, Currency: "usd"}, kernel.ErrInvalidCurrency},
		"no currency":     {dto.UpsertProductInput{SKU: "A", Name: "Apple", Amount: 1}, kernel.ErrInvalidCurrency},
		"empty sku":       {dto.UpsertProductInput{Name: "Apple", Amount: 1, Currency: "USD"}, product.ErrInvalidSKU},
		"empty name":      {dto.UpsertProductInput{SKU: "A", Amount: 1, Currency: "USD"}, product.ErrInvalidName},
	} {
		if _, err := f.upsert.Execute(c, tc.in); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}
	if _, err := f.get.Execute(c, dto.GetProductInput{SKU: "A"}); !errors.Is(err, product.ErrNotFound) {
		t.Fatalf("a refused upsert created the product: %v", err)
	}

	// a refused update leaves the stored product alone
	f.put(t, "A", 150, "USD")
	if _, err := f.upsert.Execute(c, dto.UpsertProductInput{SKU: "A", Name: "Apple", Amount: -5, Currency: "USD"}); err == nil {
		t.Fatal("negative reprice accepted")
	}
	if got, _ := f.get.Execute(c, dto.GetProductInput{SKU: "A"}); got.Price.Amount != 150 {
		t.Fatalf("price = %d after a refused reprice, want 150", got.Price.Amount)
	}
}

func TestGetProduct(t *testing.T) {
	f := newFixture()
	if _, err := f.get.Execute(context.Background(), dto.GetProductInput{SKU: ""}); !errors.Is(err, product.ErrInvalidSKU) {
		t.Fatalf("empty sku: %v", err)
	}
	if _, err := f.get.Execute(context.Background(), dto.GetProductInput{SKU: "nope"}); !errors.Is(err, product.ErrNotFound) {
		t.Fatalf("unknown sku: %v", err)
	}
}

func TestDeactivateProduct(t *testing.T) {
	f := newFixture()
	c := context.Background()
	f.put(t, "A", 150, "USD")

	f.now = f.now.Add(time.Hour)
	out, err := f.deactivate.Execute(c, dto.DeactivateProductInput{SKU: "A"})
	if err != nil || out.Active {
		t.Fatalf("deactivate = %+v, %v", out, err)
	}
	again, err := f.deactivate.Execute(c, dto.DeactivateProductInput{SKU: "A"})
	if err != nil || again.Active || !again.UpdateTime.Equal(out.UpdateTime) {
		t.Fatalf("second deactivate = %+v, %v; want success with nothing changed", again, err)
	}
	if _, err = f.deactivate.Execute(c, dto.DeactivateProductInput{SKU: "nope"}); !errors.Is(err, product.ErrNotFound) {
		t.Fatalf("unknown sku: %v", err)
	}
	if _, err = f.deactivate.Execute(c, dto.DeactivateProductInput{}); !errors.Is(err, product.ErrInvalidSKU) {
		t.Fatalf("empty sku: %v", err)
	}
}

func TestListProducts_PrefixPaging(t *testing.T) {
	f := newFixture()
	c := context.Background()
	for i := range 5 {
		f.put(t, fmt.Sprintf("P-%d", i), 10, "USD")
	}
	f.put(t, "Q-1", 10, "USD")

	var seen []string
	token := ""
	for pages := 0; ; pages++ {
		out, err := f.list.Execute(c, dto.ListProductsInput{PageSize: 2, PageToken: token, SKUPrefix: "P-"})
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range out.Products {
			seen = append(seen, p.SKU)
		}
		if token = out.NextPageToken; token == "" {
			break
		}
		if pages > 5 {
			t.Fatal("paging does not terminate")
		}
	}
	if want := []string{"P-0", "P-1", "P-2", "P-3", "P-4"}; !slices.Equal(seen, want) {
		t.Fatalf("seen = %v, want %v", seen, want)
	}

	// the token is bound to the prefix it was minted under (#79)
	first, _ := f.list.Execute(c, dto.ListProductsInput{PageSize: 2, SKUPrefix: "P-"})
	if first.NextPageToken == "" {
		t.Fatal("want a next page token")
	}
	for _, prefix := range []string{"", "Q"} {
		if _, err := f.list.Execute(c, dto.ListProductsInput{PageSize: 2, PageToken: first.NextPageToken, SKUPrefix: prefix}); !errors.Is(err, pagination.ErrInvalidToken) {
			t.Errorf("token continued with prefix %q: %v, want ErrInvalidToken", prefix, err)
		}
	}
	if _, err := f.list.Execute(c, dto.ListProductsInput{PageToken: "garbage"}); !errors.Is(err, pagination.ErrInvalidToken) {
		t.Fatalf("garbage token: %v", err)
	}
	if _, err := f.list.Execute(c, dto.ListProductsInput{PageSize: -1}); err == nil {
		t.Fatal("negative page size accepted")
	}
	if out, _ := f.list.Execute(c, dto.ListProductsInput{PageSize: 1000}); len(out.Products) != 6 {
		t.Fatalf("a page size above the maximum is clamped, not refused: got %d products", len(out.Products))
	}
	if out, _ := f.list.Execute(c, dto.ListProductsInput{}); len(out.Products) != 6 {
		t.Fatalf("default page: %d products, want all 6", len(out.Products))
	}
}

func TestQuotePrices_TellsMissingFromInactive(t *testing.T) {
	f := newFixture()
	c := context.Background()
	f.put(t, "A", 150, "USD")
	f.put(t, "B", 90, "EUR")
	f.put(t, "OLD", 5, "USD")
	if _, err := f.deactivate.Execute(c, dto.DeactivateProductInput{SKU: "OLD"}); err != nil {
		t.Fatal(err)
	}

	out, err := f.quote.Execute(c, dto.QuotePricesInput{SKUs: []string{"B", "NOPE", "A", "OLD", "A", "GONE", "NOPE"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Prices) != 2 || out.Prices["A"] != (dto.PriceOutput{Amount: 150, Currency: "USD"}) || out.Prices["B"] != (dto.PriceOutput{Amount: 90, Currency: "EUR"}) {
		t.Fatalf("prices = %v", out.Prices)
	}
	if want := []string{"NOPE", "GONE"}; !slices.Equal(out.Missing, want) {
		t.Fatalf("missing = %v, want %v (input order, once each)", out.Missing, want)
	}
	if want := []string{"OLD"}; !slices.Equal(out.Inactive, want) {
		t.Fatalf("inactive = %v, want %v", out.Inactive, want)
	}
	if _, quoted := out.Prices["OLD"]; quoted {
		t.Fatal("an inactive product was quoted")
	}

	// a reprice is visible to the next quote
	f.put(t, "A", 175, "USD")
	if out, _ = f.quote.Execute(c, dto.QuotePricesInput{SKUs: []string{"A"}}); out.Prices["A"].Amount != 175 {
		t.Fatalf("quote after reprice = %v", out.Prices)
	}

	empty, err := f.quote.Execute(c, dto.QuotePricesInput{})
	if err != nil || len(empty.Prices) != 0 || len(empty.Missing) != 0 || len(empty.Inactive) != 0 {
		t.Fatalf("no skus = %+v, %v", empty, err)
	}
	if _, err = f.quote.Execute(c, dto.QuotePricesInput{SKUs: []string{"A", ""}}); !errors.Is(err, product.ErrInvalidSKU) {
		t.Fatalf("empty sku in the list: %v", err)
	}
}
