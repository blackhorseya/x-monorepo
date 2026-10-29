package apikey

import (
	"context"
	"time"
)

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
	// TouchLastUsed stamps last_used_at = at unless it was already stamped
	// less than minInterval before at, and reports whether it wrote. It is a
	// single conditional write that does not touch Version, so it can neither
	// make a concurrent Save stale nor be undone by one, and a second replica
	// racing the same key loses instead of writing twice. A key that does not
	// exist is false, nil: there is nothing to stamp.
	TouchLastUsed(c context.Context, id ID, at time.Time, minInterval time.Duration) (bool, error)
}
