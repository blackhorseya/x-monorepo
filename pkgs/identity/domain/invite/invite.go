// Package invite is the Invite aggregate: an admin's pre-binding of an email
// to an account. The person signs in at the IdP and claims it; the claim binds
// their (issuer, subject) and the invite is spent. It holds no secret.
package invite

import (
	"context"
	"errors"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

// TTL is how long an invite can be claimed.
const TTL = 7 * 24 * time.Hour

var (
	ErrInvalidID        = kernel.NewError(kernel.KindInvalidArgument, "invite.invalid_id", "invite id must not be empty")
	ErrInvalidEmail     = kernel.NewError(kernel.KindInvalidArgument, "invite.invalid_email", "invite email is not a valid address")
	ErrInvalidAccount   = kernel.NewError(kernel.KindInvalidArgument, "invite.invalid_account", "invite must name an account")
	ErrNotFound         = kernel.NewError(kernel.KindNotFound, "invite.not_found", "no invite for this email")
	ErrExpired          = kernel.NewError(kernel.KindFailedPrecondition, "invite.expired", "invite has expired")
	ErrAlreadyClaimed   = kernel.NewError(kernel.KindFailedPrecondition, "invite.already_claimed", "invite has already been used")
	ErrEmailUnverified  = kernel.NewError(kernel.KindFailedPrecondition, "invite.email_unverified", "the identity provider has not verified this email")
	ErrEmailMismatch    = kernel.NewError(kernel.KindFailedPrecondition, "invite.email_mismatch", "token email does not match the invite")
	ErrStale            = errors.New("invite: reload and retry")
	errNotImplementedIn = errors.New("not implemented")
)

// ID identifies an Invite.
type ID string

func (x ID) String() string { return string(x) }

// Invite is the aggregate root. version is the optimistic-concurrency token.
type Invite struct {
	id        ID
	email     string
	accountID string
	createdAt time.Time
	expiresAt time.Time
	claimedAt time.Time
	version   int64
}

// New creates a pending invite valid for TTL from now.
func New(id ID, email, accountID string, now time.Time) (*Invite, error) {
	return nil, errNotImplementedIn
}

// NormalizeEmail trims and lower-cases an address for comparison.
func NormalizeEmail(email string) (string, error) { return "", errNotImplementedIn }

func (x *Invite) ID() ID               { return x.id }
func (x *Invite) Email() string        { return x.email }
func (x *Invite) AccountID() string    { return x.accountID }
func (x *Invite) CreatedAt() time.Time { return x.createdAt }
func (x *Invite) ExpiresAt() time.Time { return x.expiresAt }
func (x *Invite) ClaimedAt() time.Time { return x.claimedAt }
func (x *Invite) Version() int64       { return x.version }

// Claim spends the invite for a token whose email is verified and matches.
func (x *Invite) Claim(email string, emailVerified bool, now time.Time) error {
	return errNotImplementedIn
}

// ReconstituteParams carry persisted state back into the aggregate.
type ReconstituteParams struct {
	ID        ID
	Email     string
	AccountID string
	CreatedAt time.Time
	ExpiresAt time.Time
	ClaimedAt time.Time
	Version   int64
}

// Reconstitute rebuilds an Invite from persistence. Only repositories call it.
func Reconstitute(p ReconstituteParams) (*Invite, error) { return nil, errNotImplementedIn }

// Repository persists Invite.
type Repository interface {
	// Add inserts a never-saved invite.
	Add(c context.Context, i *Invite) error
	// FindLatestByEmail returns the newest invite for the (normalized) email,
	// locking its row for the rest of the transaction; ErrNotFound when none.
	FindLatestByEmail(c context.Context, email string) (*Invite, error)
	// Save updates a loaded invite, comparing Version: ErrStale when another writer won.
	Save(c context.Context, i *Invite) error
}
