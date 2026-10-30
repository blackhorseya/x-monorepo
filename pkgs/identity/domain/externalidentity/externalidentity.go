// Package externalidentity binds an identity that an external OIDC issuer
// vouches for, (issuer, subject), to exactly one Account. The binding key is
// never the email: an email can change or be re-assigned by the IdP.
package externalidentity

import (
	"context"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

var (
	ErrInvalidIssuer  = kernel.NewError(kernel.KindInvalidArgument, "identity.invalid_issuer", "issuer must not be empty")
	ErrInvalidSubject = kernel.NewError(kernel.KindInvalidArgument, "identity.invalid_subject", "subject must not be empty")
	ErrInvalidAccount = kernel.NewError(kernel.KindInvalidArgument, "identity.invalid_binding_account", "binding must name an account")
	ErrInvalidMapping = kernel.NewError(kernel.KindInvalidArgument, "identity.invalid_mapping", "mapping must be bound or auto_provision")
	// ErrNotFound: the (issuer, subject) is not bound to any account.
	ErrNotFound = kernel.NewError(kernel.KindNotFound, "identity.unbound", "external identity is not bound to an account")
	// ErrDuplicate: the (issuer, subject) is already bound.
	ErrDuplicate = kernel.NewError(kernel.KindConflict, "identity.already_bound", "external identity is already bound to an account")
)

// Mapping is how an issuer's unknown identities are treated.
type Mapping uint8

const (
	// MappingBound (the default, fail-closed): an identity must already be bound.
	MappingBound Mapping = iota + 1
	// MappingAutoProvision: the first sight of an identity creates an account
	// without any role and binds it.
	MappingAutoProvision
)

func (m Mapping) String() string {
	switch m {
	case MappingBound:
		return "bound"
	case MappingAutoProvision:
		return "auto_provision"
	default:
		return "unknown"
	}
}

// ParseMapping parses a configured value; "" is MappingBound, the fail-closed default.
func ParseMapping(s string) (Mapping, error) {
	switch s {
	case "", "bound":
		return MappingBound, nil
	case "auto_provision":
		return MappingAutoProvision, nil
	default:
		return 0, ErrInvalidMapping
	}
}

// Identity is one binding.
type Identity struct {
	issuer    string
	subject   string
	accountID string
	createdAt time.Time
}

// New validates and creates a binding.
func New(issuer, subject, accountID string, now time.Time) (*Identity, error) {
	switch {
	case issuer == "":
		return nil, ErrInvalidIssuer
	case subject == "":
		return nil, ErrInvalidSubject
	case accountID == "":
		return nil, ErrInvalidAccount
	}

	return &Identity{issuer: issuer, subject: subject, accountID: accountID, createdAt: now}, nil
}

func (x *Identity) Issuer() string       { return x.issuer }
func (x *Identity) Subject() string      { return x.subject }
func (x *Identity) AccountID() string    { return x.accountID }
func (x *Identity) CreatedAt() time.Time { return x.createdAt }

// Repository persists bindings.
type Repository interface {
	// Find returns ErrNotFound when (issuer, subject) is not bound.
	Find(c context.Context, issuer, subject string) (*Identity, error)
	// Add inserts a binding; ErrDuplicate when (issuer, subject) is bound.
	Add(c context.Context, i *Identity) error
	// ListByAccount returns every binding of the account, newest first
	// (created_at desc, issuer, subject). An account holds a handful, so there
	// is no paging.
	ListByAccount(c context.Context, accountID string) ([]*Identity, error)
}
