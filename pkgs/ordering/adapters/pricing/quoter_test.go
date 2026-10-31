package pricing_test

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	catalogv1 "github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1/catalogv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/pricing"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/port"
	commonv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/common/v1"
)

// fakeCatalog is the catalog's contract, as the adapter sees it: only QuotePrices is used.
type fakeCatalog struct {
	catalogv1connect.UnimplementedCatalogServiceHandler
	got []string
	res *catalogv1.QuotePricesResponse
	err error
}

func (f *fakeCatalog) QuotePrices(_ context.Context, req *connect.Request[catalogv1.QuotePricesRequest]) (*connect.Response[catalogv1.QuotePricesResponse], error) {
	f.got = req.Msg.GetSkus()
	if f.err != nil {
		return nil, f.err
	}

	return connect.NewResponse(f.res), nil
}

func TestQuoter_MapsTheCatalogsAnswer(t *testing.T) {
	cat := &fakeCatalog{res: &catalogv1.QuotePricesResponse{
		Prices:   map[string]*commonv1.Money{"A": {AmountMinor: 500, Currency: "USD"}, "B": {AmountMinor: 25, Currency: "EUR"}},
		Missing:  []string{"NOPE"},
		Inactive: []string{"OLD"},
	}}
	q := pricing.NewQuoter(cat)

	got, err := q.QuotePrices(context.Background(), []string{"A", "B", "NOPE", "OLD"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.got) != 4 || cat.got[0] != "A" {
		t.Fatalf("the catalog was asked %v", cat.got)
	}
	if got.Prices["A"] != (dto.Money{AmountMinor: 500, Currency: "USD"}) || got.Prices["B"] != (dto.Money{AmountMinor: 25, Currency: "EUR"}) || len(got.Prices) != 2 {
		t.Fatalf("prices = %v", got.Prices)
	}
	if len(got.Missing) != 1 || got.Missing[0] != "NOPE" || len(got.Inactive) != 1 || got.Inactive[0] != "OLD" {
		t.Fatalf("missing=%v inactive=%v", got.Missing, got.Inactive)
	}
}

// A catalog that fails is an infrastructure failure of this call, never a priced order and
// never a domain error (which would read as the caller's mistake).
func TestQuoter_CatalogFailureKeepsItsCause(t *testing.T) {
	boom := connect.NewError(connect.CodeUnavailable, errors.New("catalog down"))
	_, err := pricing.NewQuoter(&fakeCatalog{err: boom}).QuotePrices(context.Background(), []string{"A"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the catalog's error wrapped", err)
	}
}

func TestQuoter_NoSKUsAsksNothing(t *testing.T) {
	cat := &fakeCatalog{res: &catalogv1.QuotePricesResponse{}}
	got, err := pricing.NewQuoter(cat).QuotePrices(context.Background(), nil)
	if err != nil || len(got.Prices) != 0 || cat.got != nil {
		t.Fatalf("got %+v, %v; asked %v", got, err, cat.got)
	}
}

// A catalog that does not answer is not "an order we cannot price": the quoter says so with
// port.ErrCatalogUnavailable (keeping the cause), so CreateOrder can fail closed with UNAVAILABLE.
// Anything the catalog answered with is not that.
func TestQuoter_ACatalogThatDoesNotAnswerIsUnavailable(t *testing.T) {
	unavailable := connect.NewError(connect.CodeUnavailable, errors.New("the catalog could not be reached"))
	q := pricing.NewQuoter(&fakeCatalog{err: unavailable})

	_, err := q.QuotePrices(context.Background(), []string{"A"})
	if !errors.Is(err, port.ErrCatalogUnavailable) || !errors.Is(err, unavailable) {
		t.Fatalf("err = %v, want one wrapping port.ErrCatalogUnavailable and the cause", err)
	}

	for _, code := range []connect.Code{connect.CodeInvalidArgument, connect.CodeNotFound, connect.CodeFailedPrecondition} {
		answered := connect.NewError(code, errors.New("the catalog answered"))
		_, err := pricing.NewQuoter(&fakeCatalog{err: answered}).QuotePrices(context.Background(), []string{"A"})
		if err == nil || errors.Is(err, port.ErrCatalogUnavailable) {
			t.Fatalf("code %s: err = %v, want an error that is not ErrCatalogUnavailable", code, err)
		}
	}
}
