// Package apikey is the APIKey aggregate of the identity bounded context: one
// credential of an account. The secret itself is never kept — only its
// SHA-256, which is also how a presented key is looked up — so a database
// dump does not leak keys and the comparison is a map lookup, not a string
// compare. It imports only the shared kernel.
package apikey

import (
	"crypto/sha256"
	"time"
)

// ID identifies an APIKey; it is the principal's CredentialID once the key resolves.
type ID string

func (x ID) String() string { return string(x) }

// MinSecretLen is the shortest secret New accepts: the generator makes 32
// random bytes (43 base64url characters) behind a short prefix, so anything
// below this is a bug, not a weak key.
const MinSecretLen = 32

// PrefixLen is how many leading characters of the secret are kept in clear,
// to tell keys apart in a listing and in a client's config (`xk_ab12…`).
const PrefixLen = 8

// Hash is the stored form of a secret: HashSecret(secret).
type Hash [sha256.Size]byte

// HashSecret is the one way a secret turns into what is stored and looked up.
func HashSecret(secret string) Hash { return sha256.Sum256([]byte(secret)) }

// APIKey is the aggregate root. version is the optimistic-concurrency token
// repositories compare on Save: 0 for a key never saved, then the number of saves.
type APIKey struct {
	id        ID
	accountID string
	name      string
	prefix    string
	hash      Hash
	status    Status
	version   int64
	createdAt time.Time
	expiresAt time.Time // zero: never
	revokedAt time.Time // zero: not revoked
	// lastUsedAt is when the key last resolved, as far as the (throttled,
	// best-effort) writer got: zero means never. It is stamped by
	// Repository.TouchLastUsed, outside the optimistic lock, so no use of the
	// aggregate sets it.
	lastUsedAt time.Time
}

// New mints an ACTIVE key for accountID from a freshly generated secret.
// expiresAt zero means the key never expires; otherwise it must be after now.
func New(id ID, accountID, name, secret string, expiresAt, now time.Time) (*APIKey, error) {
	if id == "" {
		return nil, ErrInvalidID
	}
	if accountID == "" {
		return nil, ErrInvalidAccountID
	}
	if name == "" {
		return nil, ErrInvalidName
	}
	if len(secret) < MinSecretLen {
		return nil, ErrInvalidSecret
	}
	if !expiresAt.IsZero() && !expiresAt.After(now) {
		return nil, ErrInvalidExpiry
	}

	return &APIKey{
		id: id, accountID: accountID, name: name,
		prefix: secret[:PrefixLen], hash: HashSecret(secret),
		status: StatusActive, createdAt: now, expiresAt: expiresAt,
	}, nil
}

// ReconstituteParams carry persisted state back into the aggregate, as stored.
type ReconstituteParams struct {
	ID        ID
	AccountID string
	Name      string
	Prefix    string
	Hash      Hash
	Status    Status
	Version   int64
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt time.Time
	// LastUsedAt zero: never used.
	LastUsedAt time.Time
}

// Reconstitute rebuilds an APIKey from persistence. Only repositories should call it.
func Reconstitute(p ReconstituteParams) (*APIKey, error) {
	if p.ID == "" {
		return nil, ErrInvalidID
	}
	if p.AccountID == "" {
		return nil, ErrInvalidAccountID
	}
	if p.Name == "" {
		return nil, ErrInvalidName
	}
	if p.Status != StatusActive && p.Status != StatusRevoked {
		return nil, ErrInvalidStatus
	}

	return &APIKey{
		id: p.ID, accountID: p.AccountID, name: p.Name, prefix: p.Prefix, hash: p.Hash,
		status: p.Status, version: p.Version, createdAt: p.CreatedAt, expiresAt: p.ExpiresAt, revokedAt: p.RevokedAt, lastUsedAt: p.LastUsedAt,
	}, nil
}

func (x *APIKey) ID() ID               { return x.id }
func (x *APIKey) AccountID() string    { return x.accountID }
func (x *APIKey) Name() string         { return x.name }
func (x *APIKey) Prefix() string       { return x.prefix }
func (x *APIKey) Hash() Hash           { return x.hash }
func (x *APIKey) Status() Status       { return x.status }
func (x *APIKey) Version() int64       { return x.version }
func (x *APIKey) CreatedAt() time.Time { return x.createdAt }
func (x *APIKey) ExpiresAt() time.Time { return x.expiresAt }
func (x *APIKey) RevokedAt() time.Time { return x.revokedAt }

// LastUsedAt is zero for a key that never resolved.
func (x *APIKey) LastUsedAt() time.Time { return x.lastUsedAt }

// Usable reports why the key must not resolve right now (ErrRevoked,
// ErrExpired), or nil. The account's status is the account's business.
func (x *APIKey) Usable(now time.Time) error {
	if x.status == StatusRevoked {
		return ErrRevoked
	}
	if !x.expiresAt.IsZero() && !now.Before(x.expiresAt) {
		return ErrExpired
	}

	return nil
}

// Revoke ends the key for good. The row stays, so a listing still shows who
// had which key until when.
func (x *APIKey) Revoke(now time.Time) error {
	if x.status == StatusRevoked {
		return ErrAlreadyRevoked
	}
	x.status = StatusRevoked
	x.revokedAt = now

	return nil
}
