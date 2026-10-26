package apikey

import "context"

// Repository persists APIKey, one row per id; the hash is unique too.
type Repository interface {
	// FindByID returns ErrNotFound when no key has the id.
	FindByID(c context.Context, id ID) (*APIKey, error)
	// FindByHash is the auth path: the hash of a presented secret, or ErrNotFound.
	FindByHash(c context.Context, h Hash) (*APIKey, error)
	// Add inserts a never-saved key (Version 0); ErrDuplicate when the id or
	// the hash exists.
	Add(c context.Context, k *APIKey) error
	// Save updates a loaded key, comparing Version: ErrStale when another
	// writer won. The stored version advances, the in-memory aggregate's does
	// not: reload before saving it again.
	Save(c context.Context, k *APIKey) error
	// ListByAccount returns every key of the account, revoked ones included,
	// newest first (created_at desc, id desc). An account has a handful of
	// keys, so there is no paging.
	ListByAccount(c context.Context, accountID string) ([]*APIKey, error)
}
