package order

import "github.com/blackhorseya/x-monorepo/pkgs/kernel"

// LineItem is a value object inside the Order aggregate.
type LineItem struct {
	sku       string
	quantity  int32
	unitPrice kernel.Money
}

// NewLineItem validates and creates a LineItem.
func NewLineItem(sku string, quantity int32, unitPrice kernel.Money) (LineItem, error) {
	if sku == "" {
		return LineItem{}, ErrInvalidSKU
	}
	if quantity <= 0 {
		return LineItem{}, ErrInvalidQuantity
	}

	return LineItem{sku: sku, quantity: quantity, unitPrice: unitPrice}, nil
}

func (x LineItem) SKU() string             { return x.sku }
func (x LineItem) Quantity() int32         { return x.quantity }
func (x LineItem) UnitPrice() kernel.Money { return x.unitPrice }

// Subtotal returns unitPrice * quantity.
func (x LineItem) Subtotal() kernel.Money { return x.unitPrice.Multiply(int64(x.quantity)) }
