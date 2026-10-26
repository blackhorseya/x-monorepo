package account

import "context"

// ListParams pages a listing ascending by id. Limit is already clamped by the
// caller; After is "" for the first page.
type ListParams struct {
	Limit int
	After ID
}

// Repository persists Account, one row per id.
type Repository interface {
	// FindByID returns ErrNotFound when no account has the id.
	FindByID(c context.Context, id ID) (*Account, error)
	// Add inserts a never-saved account (Version 0); ErrDuplicate when the id exists.
	Add(c context.Context, a *Account) error
	// Save updates a loaded account, comparing Version: ErrStale when another
	// writer won. The stored version advances, the in-memory aggregate's does
	// not: reload before saving it again.
	Save(c context.Context, a *Account) error
	// List returns up to p.Limit accounts with id > p.After, ascending, and the
	// id to continue from, or "" when this is the last page.
	List(c context.Context, p ListParams) ([]*Account, ID, error)
}
