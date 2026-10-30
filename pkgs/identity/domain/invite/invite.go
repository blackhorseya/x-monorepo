// Package invite is the Invite aggregate: an admin's pre-binding of an email
// to an account. The person signs in at the IdP and claims it; the claim binds
// their (issuer, subject) and the invite is spent. It holds no secret.
package invite

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

// TTL is how long an invite can be claimed.
const TTL = 7 * 24 * time.Hour

var (
	ErrInvalidID       = kernel.NewError(kernel.KindInvalidArgument, "invite.invalid_id", "invite id must not be empty")
	ErrInvalidEmail    = kernel.NewError(kernel.KindInvalidArgument, "invite.invalid_email", "invite email is not a valid address")
	ErrInvalidAccount  = kernel.NewError(kernel.KindInvalidArgument, "invite.invalid_account", "invite must name an account")
	ErrNotFound        = kernel.NewError(kernel.KindNotFound, "invite.not_found", "no invite for this email")
	ErrExpired         = kernel.NewError(kernel.KindFailedPrecondition, "invite.expired", "invite has expired")
	ErrAlreadyClaimed  = kernel.NewError(kernel.KindFailedPrecondition, "invite.already_claimed", "invite has already been used")
	ErrEmailUnverified = kernel.NewError(kernel.KindFailedPrecondition, "invite.email_unverified", "the identity provider has not verified this email")
	ErrEmailMismatch   = kernel.NewError(kernel.KindFailedPrecondition, "invite.email_mismatch", "token email does not match the invite")
	// ErrStale is returned by Repository.Save when the row changed since it was
	// loaded. It wraps kernel.ErrConcurrentUpdate: rpc answers Aborted.
	ErrStale = fmt.Errorf("invite: reload and retry: %w", kernel.ErrConcurrentUpdate)
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
	if id == "" {
		return nil, ErrInvalidID
	}
	email, err := NormalizeEmail(email)
	if err != nil {
		return nil, err
	}
	if accountID == "" {
		return nil, ErrInvalidAccount
	}

	return &Invite{id: id, email: email, accountID: accountID, createdAt: now, expiresAt: now.Add(TTL)}, nil
}

// NormalizeEmail trims and lower-cases an address for comparison, and refuses
// what cannot be a plain address (exactly one "@", something on both sides, no
// whitespace). Matching is on this form only; the IdP's verification is what
// makes it trustworthy.
func NormalizeEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || domain == "" || strings.Contains(domain, "@") || strings.ContainsAny(email, " \t\r\n<>") {
		return "", ErrInvalidEmail
	}

	return email, nil
}

func (x *Invite) ID() ID               { return x.id }
func (x *Invite) Email() string        { return x.email }
func (x *Invite) AccountID() string    { return x.accountID }
func (x *Invite) CreatedAt() time.Time { return x.createdAt }
func (x *Invite) ExpiresAt() time.Time { return x.expiresAt }
func (x *Invite) ClaimedAt() time.Time { return x.claimedAt }
func (x *Invite) Version() int64       { return x.version }

// Status is where the invite stands at now. A spent invite stays CLAIMED after
// its expiry: the claim happened, and expiry only matters to one still waiting.
type Status uint8

const (
	StatusPending Status = iota + 1
	StatusClaimed
	StatusExpired
)

func (x Status) String() string {
	switch x {
	case StatusPending:
		return "PENDING"
	case StatusClaimed:
		return "CLAIMED"
	case StatusExpired:
		return "EXPIRED"
	default:
		return "UNKNOWN"
	}
}

// Status judges the invite at now, in the order Claim refuses: claimed, then
// expired (the expiry instant itself is already expired), else pending.
func (x *Invite) Status(now time.Time) Status {
	switch {
	case !x.claimedAt.IsZero():
		return StatusClaimed
	case !now.Before(x.expiresAt):
		return StatusExpired
	default:
		return StatusPending
	}
}

// Claim spends the invite for a token whose email is verified and matches.
//
// The refusals, in order: already spent, expired, email not verified by the
// IdP, email different from the invite's. A refused claim changes nothing.
func (x *Invite) Claim(email string, emailVerified bool, now time.Time) error {
	if !x.claimedAt.IsZero() {
		return ErrAlreadyClaimed
	}
	if !now.Before(x.expiresAt) {
		return ErrExpired
	}
	if !emailVerified {
		return ErrEmailUnverified
	}
	if normalized, err := NormalizeEmail(email); err != nil || normalized != x.email {
		return ErrEmailMismatch
	}
	x.claimedAt = now

	return nil
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
func Reconstitute(p ReconstituteParams) (*Invite, error) {
	if p.ID == "" {
		return nil, ErrInvalidID
	}
	if _, err := NormalizeEmail(p.Email); err != nil {
		return nil, err
	}
	if p.AccountID == "" {
		return nil, ErrInvalidAccount
	}

	return &Invite{id: p.ID, email: p.Email, accountID: p.AccountID, createdAt: p.CreatedAt, expiresAt: p.ExpiresAt, claimedAt: p.ClaimedAt, version: p.Version}, nil
}

// Repository persists Invite.
type Repository interface {
	// Add inserts a never-saved invite.
	Add(c context.Context, i *Invite) error
	// FindLatestByEmail returns the newest invite for the (normalized) email,
	// locking its row for the rest of the transaction; ErrNotFound when none.
	FindLatestByEmail(c context.Context, email string) (*Invite, error)
	// ListByAccount returns every invite of the account, spent and expired ones
	// included, newest first (created_at desc, id desc). It does not lock rows.
	ListByAccount(c context.Context, accountID string) ([]*Invite, error)
	// Save updates a loaded invite, comparing Version: ErrStale when another writer won.
	Save(c context.Context, i *Invite) error
}
