// Package product is the Product aggregate of the catalog bounded context:
// what one SKU sells for. It imports only the shared kernel.
package product

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

// Field limits, mirrored by the buf.validate rules of CatalogService and the
// column checks of the schema.
const (
	maxSKUBytes  = 64
	maxNameRunes = 200
)

// assigned is currencyCodes as a set.
var assigned = func() map[string]struct{} {
	set := make(map[string]struct{}, len(currencyCodes))
	for _, c := range currencyCodes {
		set[c] = struct{}{}
	}

	return set
}()

// CurrencyCodes lists the ISO 4217 codes a Product may be priced in, ascending (a copy).
func CurrencyCodes() []string { return append([]string(nil), currencyCodes...) }

// SKU identifies a Product.
type SKU string

func (x SKU) String() string { return string(x) }

// Product is the aggregate root. The price carries its own currency, so two
// products may be priced in different ones. An inactive product is kept (its
// SKU may still be on old orders) but is no longer quoted.
// version is the optimistic-concurrency token repositories compare on Save:
// 0 for a product never saved, then the number of saves.
type Product struct {
	sku       SKU
	name      string
	price     kernel.Money
	active    bool
	version   int64
	updatedAt time.Time
}

// New creates an active Product.
func New(sku SKU, name string, price kernel.Money, now time.Time) (*Product, error) {
	if err := validate(sku, name, price); err != nil {
		return nil, err
	}

	return &Product{sku: sku, name: name, price: price, active: true, updatedAt: now}, nil
}

// ReconstituteParams carry persisted state back into the aggregate, as stored.
type ReconstituteParams struct {
	SKU       SKU
	Name      string
	Price     kernel.Money
	Active    bool
	Version   int64
	UpdatedAt time.Time
}

// Reconstitute rebuilds a Product from persistence. Only repositories should call it.
func Reconstitute(p ReconstituteParams) (*Product, error) {
	if err := validate(p.SKU, p.Name, p.Price); err != nil {
		return nil, err
	}

	return &Product{sku: p.SKU, name: p.Name, price: p.Price, active: p.Active, version: p.Version, updatedAt: p.UpdatedAt}, nil
}

func validate(sku SKU, name string, price kernel.Money) error {
	if sku == "" || len(sku) > maxSKUBytes {
		return ErrInvalidSKU
	}
	if err := validateName(name); err != nil {
		return err
	}

	return validatePrice(price)
}

func validateName(name string) error {
	if strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > maxNameRunes {
		return ErrInvalidName
	}

	return nil
}

// validatePrice holds the price to what kernel.NewMoney guarantees. Money's fields are private,
// but its zero value, Multiply (which can go negative) and ZeroMoney (any string) all produce
// values NewMoney would have refused, so the aggregate re-checks instead of trusting the type;
// then the currency must be an assigned ISO 4217 code, which the kernel's shape check cannot say.
func validatePrice(price kernel.Money) error {
	if _, err := kernel.NewMoney(price.Amount(), price.Currency()); err != nil {
		return ErrInvalidPrice
	}
	if _, ok := assigned[price.Currency()]; !ok {
		return ErrUnsupportedCurrency
	}

	return nil
}

func (x *Product) SKU() SKU             { return x.sku }
func (x *Product) Name() string         { return x.name }
func (x *Product) Price() kernel.Money  { return x.price }
func (x *Product) Active() bool         { return x.active }
func (x *Product) Version() int64       { return x.version }
func (x *Product) UpdatedAt() time.Time { return x.updatedAt }

// Rename changes the display name.
func (x *Product) Rename(name string, now time.Time) error {
	if err := validateName(name); err != nil {
		return err
	}

	x.name = name
	x.updatedAt = now

	return nil
}

// Reprice changes the price. The currency may change with it: it belongs to
// the product, not to the catalog.
func (x *Product) Reprice(price kernel.Money, now time.Time) error {
	if err := validatePrice(price); err != nil {
		return err
	}

	x.price = price
	x.updatedAt = now

	return nil
}

// Deactivate stops the product from being quoted. Already inactive: nothing changes.
func (x *Product) Deactivate(now time.Time) {
	if !x.active {
		return
	}

	x.active = false
	x.updatedAt = now
}

// Activate makes the product quotable again. Already active: nothing changes.
func (x *Product) Activate(now time.Time) {
	if x.active {
		return
	}

	x.active = true
	x.updatedAt = now
}
