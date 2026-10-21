package reservation

import "time"

// ListParams narrows and pages a listing, newest first (created_at desc,
// order_id desc). Limit is already clamped by the caller; After is nil for
// the first page.
type ListParams struct {
	Status *Status // nil = any
	Limit  int
	After  *Cursor
}

// Cursor is a keyset position in the canonical listing order: "everything
// strictly after this reservation". It is a domain value so repositories
// agree on the sort key; the opaque page token clients see is built from it
// in the application layer.
type Cursor struct {
	createdAt time.Time
	orderID   string
}

// NewCursor builds a Cursor; both parts are required (order id breaks
// created_at ties).
func NewCursor(createdAt time.Time, orderID string) (Cursor, error) {
	if createdAt.IsZero() || orderID == "" {
		return Cursor{}, ErrInvalidCursor
	}

	return Cursor{createdAt: createdAt, orderID: orderID}, nil
}

// CursorOf is the cursor pointing at r, i.e. the token for "the page after r".
func CursorOf(r *Reservation) Cursor {
	return Cursor{createdAt: r.CreatedAt(), orderID: r.OrderID()}
}

func (x Cursor) CreatedAt() time.Time { return x.createdAt }
func (x Cursor) OrderID() string      { return x.orderID }

// Before reports whether the row (createdAt, orderID) belongs to the page
// after x: it is older, or equally old with a smaller order id.
func (x Cursor) Before(createdAt time.Time, orderID string) bool {
	if !createdAt.Equal(x.createdAt) {
		return createdAt.Before(x.createdAt)
	}

	return orderID < x.orderID
}
