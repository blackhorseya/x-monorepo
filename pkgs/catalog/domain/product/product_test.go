package product_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/catalog/domain/product"
	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

var now = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func usd(t *testing.T, amount int64) kernel.Money {
	t.Helper()
	m, err := kernel.NewMoney(amount, "USD")
	if err != nil {
		t.Fatal(err)
	}

	return m
}

func TestNew(t *testing.T) {
	p, err := product.New("A", "Apple", usd(t, 150), now)
	if err != nil {
		t.Fatal(err)
	}
	if p.SKU() != "A" || p.SKU().String() != "A" || p.Name() != "Apple" || p.Price() != usd(t, 150) || !p.Active() || p.Version() != 0 || !p.UpdatedAt().Equal(now) {
		t.Fatalf("unexpected product %+v", p)
	}

	cases := map[string]struct {
		sku   product.SKU
		name  string
		price kernel.Money
		want  error
	}{
		"empty sku":        {"", "Apple", usd(t, 1), product.ErrInvalidSKU},
		"sku too long":     {product.SKU(strings.Repeat("x", 65)), "Apple", usd(t, 1), product.ErrInvalidSKU},
		"empty name":       {"A", "", usd(t, 1), product.ErrInvalidName},
		"blank name":       {"A", "  ", usd(t, 1), product.ErrInvalidName},
		"name too long":    {"A", strings.Repeat("界", 201), usd(t, 1), product.ErrInvalidName},
		"zero value price": {"A", "Apple", kernel.Money{}, product.ErrInvalidPrice},
	}
	for name, c := range cases {
		if _, err := product.New(c.sku, c.name, c.price, now); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
	}
	// the boundaries themselves are fine: 64-byte sku, 200-rune name, free product
	if _, err := product.New(product.SKU(strings.Repeat("x", 64)), strings.Repeat("界", 200), usd(t, 0), now); err != nil {
		t.Fatalf("boundary values: %v", err)
	}
}

func TestReconstitute(t *testing.T) {
	p, err := product.Reconstitute(product.ReconstituteParams{SKU: "A", Name: "Apple", Price: usd(t, 5), Active: false, Version: 7, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if p.Active() || p.Version() != 7 || p.Price() != usd(t, 5) {
		t.Fatalf("unexpected product %+v", p)
	}

	for name, params := range map[string]product.ReconstituteParams{
		"empty sku":  {SKU: "", Name: "Apple", Price: usd(t, 1)},
		"empty name": {SKU: "A", Name: "", Price: usd(t, 1)},
		"no price":   {SKU: "A", Name: "Apple"},
	} {
		if _, err := product.Reconstitute(params); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestReprice(t *testing.T) {
	p, _ := product.New("A", "Apple", usd(t, 150), now)
	later := now.Add(time.Minute)

	eur, _ := kernel.NewMoney(90, "EUR")
	if err := p.Reprice(eur, later); err != nil {
		t.Fatal(err)
	}
	if p.Price() != eur || !p.UpdatedAt().Equal(later) {
		t.Fatalf("after reprice: price=%v updated=%v (currency is per product, so it may change)", p.Price(), p.UpdatedAt())
	}
	if err := p.Reprice(kernel.Money{}, later.Add(time.Minute)); !errors.Is(err, product.ErrInvalidPrice) {
		t.Fatalf("zero value price: %v", err)
	}
	if p.Price() != eur || !p.UpdatedAt().Equal(later) {
		t.Fatalf("a refused reprice changed the product: %+v", p)
	}
}

func TestRename(t *testing.T) {
	p, _ := product.New("A", "Apple", usd(t, 150), now)
	later := now.Add(time.Minute)

	if err := p.Rename("Green apple", later); err != nil {
		t.Fatal(err)
	}
	if p.Name() != "Green apple" || !p.UpdatedAt().Equal(later) {
		t.Fatalf("after rename: %+v", p)
	}
	if err := p.Rename(" ", later.Add(time.Minute)); !errors.Is(err, product.ErrInvalidName) {
		t.Fatalf("blank name: %v", err)
	}
	if p.Name() != "Green apple" || !p.UpdatedAt().Equal(later) {
		t.Fatalf("a refused rename changed the product: %+v", p)
	}
}

func TestDeactivateActivate(t *testing.T) {
	p, _ := product.New("A", "Apple", usd(t, 150), now)
	t1, t2, t3 := now.Add(time.Minute), now.Add(2*time.Minute), now.Add(3*time.Minute)

	p.Deactivate(t1)
	if p.Active() || !p.UpdatedAt().Equal(t1) {
		t.Fatalf("after deactivate: %+v", p)
	}
	p.Deactivate(t2) // already inactive: nothing changes, not even the timestamp
	if p.Active() || !p.UpdatedAt().Equal(t1) {
		t.Fatalf("second deactivate: %+v", p)
	}
	p.Activate(t2)
	if !p.Active() || !p.UpdatedAt().Equal(t2) {
		t.Fatalf("after activate: %+v", p)
	}
	p.Activate(t3)
	if !p.Active() || !p.UpdatedAt().Equal(t2) {
		t.Fatalf("second activate: %+v", p)
	}
}

func TestErrStaleIsConcurrentUpdate(t *testing.T) {
	if !errors.Is(product.ErrStale, kernel.ErrConcurrentUpdate) {
		t.Fatal("ErrStale must wrap kernel.ErrConcurrentUpdate so rpc answers Aborted")
	}
	var ke *kernel.Error
	if errors.As(product.ErrStale, &ke) {
		t.Fatal("ErrStale must not be a kernel.Error")
	}
}

// Money's zero value is not the only way to hold a Money that kernel.NewMoney
// would have refused: Multiply(-1) goes negative, ZeroMoney takes any string.
// The aggregate must refuse them all, on every path that stores a price.
func TestPriceInvariants(t *testing.T) {
	negative := usd(t, 100).Multiply(-1)
	for name, price := range map[string]kernel.Money{
		"negative via Multiply": negative,
		"lower case currency":   kernel.ZeroMoney("usd"),
		"long currency":         kernel.ZeroMoney("BOGUS"),
		"short currency":        kernel.ZeroMoney("US"),
	} {
		if _, err := product.New("A", "Apple", price, now); !errors.Is(err, product.ErrInvalidPrice) {
			t.Errorf("New with %s: %v, want ErrInvalidPrice", name, err)
		}
		if _, err := product.Reconstitute(product.ReconstituteParams{SKU: "A", Name: "Apple", Price: price, Active: true}); !errors.Is(err, product.ErrInvalidPrice) {
			t.Errorf("Reconstitute with %s: %v, want ErrInvalidPrice", name, err)
		}
		p, _ := product.New("A", "Apple", usd(t, 1), now)
		if err := p.Reprice(price, now.Add(time.Minute)); !errors.Is(err, product.ErrInvalidPrice) {
			t.Errorf("Reprice with %s: %v, want ErrInvalidPrice", name, err)
		}
		if p.Price() != usd(t, 1) {
			t.Errorf("a refused Reprice with %s changed the price to %v", name, p.Price())
		}
	}
}

// The currency must be an assigned ISO 4217 code, not just three capital letters.
func TestUnassignedCurrencyIsRefused(t *testing.T) {
	const code = "product.unsupported_currency"
	hasCode := func(err error) bool {
		var ke *kernel.Error
		return errors.As(err, &ke) && ke.Code() == code
	}
	for _, c := range []string{"AAA", "ZZZ", "QQQ"} {
		m, err := kernel.NewMoney(1, c)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := product.New("A", "Apple", m, now); !hasCode(err) {
			t.Errorf("New with %s: %v, want %s", c, err, code)
		}
		p, _ := product.New("A", "Apple", usd(t, 1), now)
		if err := p.Reprice(m, now); !hasCode(err) {
			t.Errorf("Reprice with %s: %v, want %s", c, err, code)
		}
	}
	for _, c := range []string{"USD", "EUR", "JPY", "TWD"} {
		m, _ := kernel.NewMoney(1, c)
		if _, err := product.New("A", "Apple", m, now); err != nil {
			t.Errorf("New with %s: %v", c, err)
		}
	}
}

// Every whitespace Go's strings.TrimSpace removes makes a blank name; a zero
// width space is not whitespace to it.
func TestBlankNames(t *testing.T) {
	for _, name := range []string{"\t", "\n", "\v", "\f", "\r", " ", "\u0085", "\u00a0", "\u1680", "\u2003", "\u2028", "\u2029", "\u202f", "\u205f", "\u3000", " \t\u3000 "} {
		if _, err := product.New("A", name, usd(t, 1), now); !errors.Is(err, product.ErrInvalidName) {
			t.Errorf("name %q: %v, want ErrInvalidName", name, err)
		}
	}
	if _, err := product.New("A", "\u200b", usd(t, 1), now); err != nil {
		t.Errorf("zero width space: %v", err)
	}
}

func TestCurrencyCodes(t *testing.T) {
	codes := product.CurrencyCodes()
	if len(codes) < 100 {
		t.Fatalf("only %d codes", len(codes))
	}
	seen := map[string]bool{}
	for i, c := range codes {
		if _, err := kernel.NewMoney(0, c); err != nil || seen[c] || (i > 0 && codes[i-1] >= c) {
			t.Fatalf("code %q at %d: shape err=%v duplicate=%v; want distinct, ascending, 3 capital letters", c, i, err, seen[c])
		}
		seen[c] = true
	}
	codes[0] = "XXX-changed"
	if product.CurrencyCodes()[0] == "XXX-changed" {
		t.Fatal("CurrencyCodes returns the package's own slice")
	}
}
