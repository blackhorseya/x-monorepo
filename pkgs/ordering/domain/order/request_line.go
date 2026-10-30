package order

// RequestLine is one line of what a CreateOrder request asked for: a sku and how many.
// It carries no price: the price is the catalog's, and becomes part of the order only as
// the snapshot on its LineItem.
type RequestLine struct {
	sku      string
	quantity int32
}

// NewRequestLine validates and creates a RequestLine.
func NewRequestLine(sku string, quantity int32) (RequestLine, error) {
	if sku == "" {
		return RequestLine{}, ErrInvalidSKU
	}
	if quantity <= 0 {
		return RequestLine{}, ErrInvalidQuantity
	}

	return RequestLine{sku: sku, quantity: quantity}, nil
}

func (x RequestLine) SKU() string     { return x.sku }
func (x RequestLine) Quantity() int32 { return x.quantity }

// requestLines is what an order's items were asked as: its sku / quantity pairs, in order.
func requestLines(items []LineItem) []RequestLine {
	out := make([]RequestLine, 0, len(items))
	for _, it := range items {
		out = append(out, RequestLine{sku: it.sku, quantity: it.quantity})
	}

	return out
}
