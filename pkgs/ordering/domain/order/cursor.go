package order

import "time"

// Cursor is a keyset position in the canonical listing order
// (created_at desc, id desc): "everything strictly after this order".
// It is a domain value so repositories agree on the sort key; the opaque
// page token clients see is built from it in the application layer.
type Cursor struct {
	createdAt time.Time
	id        ID
}

// NewCursor builds a Cursor; both parts are required (id breaks created_at ties).
func NewCursor(createdAt time.Time, id ID) (Cursor, error) {
	if createdAt.IsZero() || id == "" {
		return Cursor{}, ErrInvalidCursor
	}

	return Cursor{createdAt: createdAt, id: id}, nil
}

// CursorOf is the cursor pointing at o, i.e. the token for "the page after o".
func CursorOf(o *Order) Cursor {
	return Cursor{createdAt: o.CreatedAt(), id: o.ID()}
}

func (x Cursor) CreatedAt() time.Time { return x.createdAt }
func (x Cursor) ID() ID               { return x.id }

// Before reports whether x sorts after (is older than) the row (createdAt, id)
// in listing order, i.e. the row belongs to the page after x.
func (x Cursor) Before(createdAt time.Time, id ID) bool {
	if !createdAt.Equal(x.createdAt) {
		return createdAt.Before(x.createdAt)
	}

	return id < x.id
}
