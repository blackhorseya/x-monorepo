package reservation

// Line is one SKU / quantity pair of a reservation (a value object).
type Line struct {
	sku      string
	quantity int32
}

func NewLine(sku string, quantity int32) (Line, error) {
	if sku == "" {
		return Line{}, ErrInvalidSKU
	}
	if quantity <= 0 {
		return Line{}, ErrInvalidQuantity
	}

	return Line{sku: sku, quantity: quantity}, nil
}

func (x Line) SKU() string     { return x.sku }
func (x Line) Quantity() int32 { return x.quantity }
