// Package account is the Account aggregate of the identity bounded context:
// who an API key speaks for, and the roles the platform's RequireRole checks.
// It imports only the shared kernel.
package account

import (
	"errors"
	"slices"
	"strings"
	"time"
)

// ID identifies an Account; it is the principal's subject once a key of the
// account resolves.
type ID string

func (x ID) String() string { return string(x) }

// Account is the aggregate root. version is the optimistic-concurrency token
// repositories compare on Save: 0 for an account never saved, then the number
// of saves.
type Account struct {
	id        ID
	name      string
	roles     []string
	status    Status
	version   int64
	createdAt time.Time
	updatedAt time.Time
	// sessionsRevokedAt: access tokens issued before it are refused. Zero: never revoked.
	sessionsRevokedAt time.Time
}

// New creates an ACTIVE account. Roles are trimmed, de-duplicated and sorted;
// an empty role is ErrInvalidRole, an empty list is fine (a key that only
// identifies).
func New(id ID, name string, roles []string, now time.Time) (*Account, error) {
	if id == "" {
		return nil, ErrInvalidID
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, ErrInvalidName
	}
	normalized, err := normalizeRoles(roles)
	if err != nil {
		return nil, err
	}

	return &Account{id: id, name: name, roles: normalized, status: StatusActive, createdAt: now, updatedAt: now}, nil
}

// ReconstituteParams carry persisted state back into the aggregate, as stored.
type ReconstituteParams struct {
	ID        ID
	Name      string
	Roles     []string
	Status    Status
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
	// SessionsRevokedAt zero: never revoked.
	SessionsRevokedAt time.Time
}

// Reconstitute rebuilds an Account from persistence. Only repositories should call it.
func Reconstitute(p ReconstituteParams) (*Account, error) {
	if p.ID == "" {
		return nil, ErrInvalidID
	}
	if p.Name == "" {
		return nil, ErrInvalidName
	}
	if p.Status != StatusActive && p.Status != StatusDisabled {
		return nil, ErrInvalidStatus
	}
	roles, err := normalizeRoles(p.Roles)
	if err != nil {
		return nil, err
	}

	return &Account{id: p.ID, name: p.Name, roles: roles, status: p.Status, version: p.Version, createdAt: p.CreatedAt, updatedAt: p.UpdatedAt}, nil
}

func (x *Account) ID() ID                       { return x.id }
func (x *Account) Name() string                 { return x.name }
func (x *Account) Roles() []string              { return slices.Clone(x.roles) }
func (x *Account) Status() Status               { return x.status }
func (x *Account) Version() int64               { return x.version }
func (x *Account) CreatedAt() time.Time         { return x.createdAt }
func (x *Account) UpdatedAt() time.Time         { return x.updatedAt }
func (x *Account) SessionsRevokedAt() time.Time { return time.Time{} }
func (x *Account) IsActive() bool               { return x.status == StatusActive }
func (x *Account) HasRole(r string) bool        { return slices.Contains(x.roles, r) }

// Disable refuses every credential of the account from now on. The keys are
// untouched: Enable brings them back.
func (x *Account) Disable(now time.Time) error {
	if x.status == StatusDisabled {
		return ErrAlreadyDisabled
	}
	x.status = StatusDisabled
	x.updatedAt = now

	return nil
}

// Enable is the inverse of Disable.
func (x *Account) Enable(now time.Time) error {
	if x.status == StatusActive {
		return ErrAlreadyActive
	}
	x.status = StatusActive
	x.updatedAt = now

	return nil
}

// RevokeSessions refuses every access token issued before now (RED stub).
func (x *Account) RevokeSessions(now time.Time) error { return errNotImplemented }

// AssignRoles replaces the roles (an empty list clears them). The change
// reaches resolved principals within the resolver cache TTL.
func (x *Account) AssignRoles(roles []string, now time.Time) error {
	normalized, err := normalizeRoles(roles)
	if err != nil {
		return err
	}
	x.roles = normalized
	x.updatedAt = now

	return nil
}

// CheckSession decides whether an access token issued at issuedAt may still be
// used by an account whose sessions were revoked at revokedAt (zero: never).
// RED stub.
func CheckSession(revokedAt, issuedAt time.Time) error { return nil }

// normalizeRoles trims, rejects empties, de-duplicates and sorts, so two
// accounts with the same roles compare equal whatever order a client sent.
func normalizeRoles(roles []string) ([]string, error) {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		r = strings.TrimSpace(r)
		if r == "" {
			return nil, ErrInvalidRole
		}
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	slices.Sort(out)

	return out, nil
}

var errNotImplemented = errors.New("not implemented")
