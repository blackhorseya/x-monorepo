// Package stock is the Stock aggregate of the inventory bounded context: the
// quantity of one SKU. It imports only the shared kernel.
package stock

import "time"

// maxSKUBytes is the longest SKU in bytes; catalog's Product has the same rule, so a stocked
// SKU can always be priced and ordered.
const maxSKUBytes = 64

// ValidateSKU is the one SKU format rule (1 to 64 bytes): New applies it, and so does anything
// that must refuse a malformed SKU before it asks another context about it.
func ValidateSKU(sku SKU) error {
	if sku == "" || len(sku) > maxSKUBytes {
		return ErrInvalidSKU
	}

	return nil
}

// SKU identifies a Stock.
type SKU string

func (x SKU) String() string { return string(x) }

// Stock is the aggregate root: on-hand minus reserved is what new orders can claim.
// version is the optimistic-concurrency token repositories compare on Save:
// 0 for a stock never saved, then the number of saves.
type Stock struct {
	sku       SKU
	onHand    int32
	reserved  int32
	version   int64
	updatedAt time.Time
}

// New creates a Stock with nothing reserved.
func New(sku SKU, onHand int32, now time.Time) (*Stock, error) {
	if err := ValidateSKU(sku); err != nil {
		return nil, err
	}
	if onHand < 0 {
		return nil, ErrNegativeQuantity
	}

	return &Stock{sku: sku, onHand: onHand, updatedAt: now}, nil
}

// ReconstituteParams carry persisted state back into the aggregate, as stored.
type ReconstituteParams struct {
	SKU       SKU
	OnHand    int32
	Reserved  int32
	Version   int64
	UpdatedAt time.Time
}

// Reconstitute rebuilds a Stock from persistence. Only repositories should call it.
func Reconstitute(p ReconstituteParams) (*Stock, error) {
	if p.SKU == "" {
		return nil, ErrInvalidSKU
	}
	if p.OnHand < 0 || p.Reserved < 0 || p.Reserved > p.OnHand {
		return nil, ErrCorruptQuantities
	}

	return &Stock{sku: p.SKU, onHand: p.OnHand, reserved: p.Reserved, version: p.Version, updatedAt: p.UpdatedAt}, nil
}

func (x *Stock) SKU() SKU             { return x.sku }
func (x *Stock) OnHand() int32        { return x.onHand }
func (x *Stock) Reserved() int32      { return x.reserved }
func (x *Stock) Available() int32     { return x.onHand - x.reserved }
func (x *Stock) Version() int64       { return x.version }
func (x *Stock) UpdatedAt() time.Time { return x.updatedAt }

// Restock sets the on-hand quantity (a count, not a delta). It can never go
// below what pending orders already hold.
func (x *Stock) Restock(onHand int32, now time.Time) error {
	if onHand < 0 {
		return ErrNegativeQuantity
	}
	if onHand < x.reserved {
		return ErrBelowReserved
	}

	x.onHand = onHand
	x.updatedAt = now

	return nil
}

// CanReserve reports whether quantity is available right now.
func (x *Stock) CanReserve(quantity int32) bool {
	return quantity > 0 && quantity <= x.Available()
}

// ReserveAll claims need[sku] from stocks[sku] for every sku, all or nothing:
// it returns the SKUs that are short (absent from stocks, or not enough
// available), in the order given, and touches no Stock in that case. When
// nothing is short every Stock has been reserved. It lives here, not on
// Reservation, because only this package may change a Stock without going
// through the fallible single-aggregate API.
func ReserveAll(stocks map[string]*Stock, skus []string, need map[string]int32, now time.Time) []string {
	var short []string
	for _, sku := range skus {
		if s, ok := stocks[sku]; !ok || !s.CanReserve(need[sku]) {
			short = append(short, sku)
		}
	}
	if len(short) > 0 {
		return short
	}

	for _, sku := range skus {
		s := stocks[sku]
		s.reserved += need[sku]
		s.updatedAt = now
	}

	return nil
}

// Commit turns a reservation into a sale: the quantity leaves both reserved
// and on-hand. Only what is reserved can be committed, so the invariant
// 0 <= reserved <= on_hand holds without a separate check.
func (x *Stock) Commit(quantity int32, now time.Time) error {
	if quantity <= 0 {
		return ErrInvalidQuantity
	}
	if quantity > x.reserved {
		return ErrCommitExceedsReserved
	}

	x.onHand -= quantity
	x.reserved -= quantity
	x.updatedAt = now

	return nil
}

// SellAll sells need[sku] from stocks[sku] for every sku, all or nothing, for
// a sale whose reservation had already been given back (OrderCompleted after
// the reservation expired): the quantity leaves on-hand straight from what is
// available. It returns the SKUs that are short (absent from stocks, or not
// enough available), in the order given, and touches no Stock in that case.
// Like ReserveAll it lives here because only this package may change a Stock
// without the fallible single-aggregate API; reserved <= on_hand still holds.
func SellAll(stocks map[string]*Stock, skus []string, need map[string]int32, now time.Time) []string {
	var short []string
	for _, sku := range skus {
		if s, ok := stocks[sku]; !ok || !s.CanReserve(need[sku]) {
			short = append(short, sku)
		}
	}
	if len(short) > 0 {
		return short
	}

	for _, sku := range skus {
		s := stocks[sku]
		s.onHand -= need[sku]
		s.updatedAt = now
	}

	return nil
}

// Release gives a reservation back.
func (x *Stock) Release(quantity int32, now time.Time) error {
	if quantity <= 0 {
		return ErrInvalidQuantity
	}
	if quantity > x.reserved {
		return ErrReleaseExceedsReserved
	}

	x.reserved -= quantity
	x.updatedAt = now

	return nil
}
