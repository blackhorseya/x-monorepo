// Package usecase holds the identity use cases: orchestration only, the
// rules live in domain/account and domain/apikey.
package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/mapper"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/pagination"
)

// CreateAccount registers who a key will speak for.
type CreateAccount struct {
	accounts account.Repository
	ids      port.IDGenerator
	clock    port.Clock
}

func NewCreateAccount(accounts account.Repository, ids port.IDGenerator, clock port.Clock) *CreateAccount {
	return &CreateAccount{accounts: accounts, ids: ids, clock: clock}
}

func (uc *CreateAccount) Execute(c context.Context, in dto.CreateAccountInput) (*dto.AccountOutput, error) {
	ctx := contextx.FromContext(c)

	id, err := uc.ids.NewID(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate account id: %w", err)
	}
	a, err := account.New(account.ID(id), in.Name, in.Roles, uc.clock.Now())
	if err != nil {
		return nil, err
	}
	if err := uc.accounts.Add(ctx, a); err != nil {
		return nil, fmt.Errorf("add account %s: %w", id, err)
	}

	return mapper.ToAccountOutput(a), nil
}

// GetAccount reads one account.
type GetAccount struct {
	accounts account.Repository
}

func NewGetAccount(accounts account.Repository) *GetAccount { return &GetAccount{accounts: accounts} }

func (uc *GetAccount) Execute(c context.Context, in dto.GetAccountInput) (*dto.AccountOutput, error) {
	if in.ID == "" {
		return nil, account.ErrInvalidID
	}
	a, err := uc.accounts.FindByID(contextx.FromContext(c), account.ID(in.ID))
	if err != nil {
		return nil, err
	}

	return mapper.ToAccountOutput(a), nil
}

// Page-size policy of ListAccounts. Clients ask, the server decides.
const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// ListAccounts pages through accounts by id ascending; the opaque token is
// minted and verified here (platform/pagination).
type ListAccounts struct {
	accounts account.Repository
}

func NewListAccounts(accounts account.Repository) *ListAccounts {
	return &ListAccounts{accounts: accounts}
}

type pageToken struct {
	ID string `json:"id"`
}

func (uc *ListAccounts) Execute(c context.Context, in dto.ListAccountsInput) (*dto.ListAccountsOutput, error) {
	ctx := contextx.FromContext(c)

	limit, err := pagination.Clamp(in.PageSize, DefaultPageSize, MaxPageSize)
	if err != nil {
		return nil, err
	}
	params := account.ListParams{Limit: limit}
	if in.PageToken != "" {
		var tok pageToken
		if err := pagination.DecodeToken(in.PageToken, &tok); err != nil {
			return nil, err
		}
		if tok.ID == "" {
			return nil, account.ErrInvalidCursor
		}
		params.After = account.ID(tok.ID)
	}

	accounts, next, err := uc.accounts.List(ctx, params)
	if err != nil {
		return nil, err
	}
	out := &dto.ListAccountsOutput{Accounts: make([]dto.AccountOutput, 0, len(accounts))}
	for _, a := range accounts {
		out.Accounts = append(out.Accounts, *mapper.ToAccountOutput(a))
	}
	if next != "" {
		if out.NextPageToken, err = pagination.EncodeToken(pageToken{ID: next.String()}); err != nil {
			return nil, err
		}
	}

	return out, nil
}

// DisableAccount refuses the account's keys from now on (within the resolver cache TTL).
// Disabling the last effective admin is account.ErrLastAdmin.
type DisableAccount struct {
	accounts account.Repository
	census   port.AdminCensus
	role     string
	clock    port.Clock
}

func NewDisableAccount(accounts account.Repository, census port.AdminCensus, adminRole string, clock port.Clock) *DisableAccount {
	return &DisableAccount{accounts: accounts, census: census, role: adminRole, clock: clock}
}

func (uc *DisableAccount) Execute(c context.Context, in dto.DisableAccountInput) (*dto.AccountOutput, error) {
	g := &adminGuard{census: uc.census, role: uc.role}

	return mutateAccount(c, uc.accounts, g, uc.clock, in.ID, func(a *account.Account, now time.Time) error { return a.Disable(now) })
}

// EnableAccount is the inverse of DisableAccount.
type EnableAccount struct {
	accounts account.Repository
	clock    port.Clock
}

func NewEnableAccount(accounts account.Repository, clock port.Clock) *EnableAccount {
	return &EnableAccount{accounts: accounts, clock: clock}
}

func (uc *EnableAccount) Execute(c context.Context, in dto.EnableAccountInput) (*dto.AccountOutput, error) {
	return mutateAccount(c, uc.accounts, nil, uc.clock, in.ID, func(a *account.Account, now time.Time) error { return a.Enable(now) })
}

// AssignRoles replaces the account's roles. Dropping the admin role from the
// last effective admin is account.ErrLastAdmin.
type AssignRoles struct {
	accounts account.Repository
	census   port.AdminCensus
	role     string
	clock    port.Clock
}

func NewAssignRoles(accounts account.Repository, census port.AdminCensus, adminRole string, clock port.Clock) *AssignRoles {
	return &AssignRoles{accounts: accounts, census: census, role: adminRole, clock: clock}
}

func (uc *AssignRoles) Execute(c context.Context, in dto.AssignRolesInput) (*dto.AccountOutput, error) {
	g := &adminGuard{census: uc.census, role: uc.role}

	return mutateAccount(c, uc.accounts, g, uc.clock, in.ID, func(a *account.Account, now time.Time) error { return a.AssignRoles(in.Roles, now) })
}

// adminGuard is what a writer that can take admin standing away needs: the
// census (taken, and locked, before anything is loaded) and the role that
// defines "effective admin".
type adminGuard struct {
	census port.AdminCensus
	role   string
}

// lock returns the effective admins and the instant they were judged at: read
// once the lock is held, so the writer's own "now" never predates it.
func (g *adminGuard) lock(ctx context.Context) ([]account.AdminStanding, time.Time, error) {
	admins, now, err := g.census.LockAdmins(ctx, g.role)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("lock admins: %w", err)
	}

	return admins, now, nil
}

// mutateAccount is load → behaviour → save, shared by the three account writes.
// With a guard it first locks the admins, then refuses a change after which
// the account is no longer an ACTIVE holder of the admin role while being the
// last effective admin.
func mutateAccount(c context.Context, accounts account.Repository, g *adminGuard, clock port.Clock, id string, fn func(*account.Account, time.Time) error) (*dto.AccountOutput, error) {
	ctx := contextx.FromContext(c)
	if id == "" {
		return nil, account.ErrInvalidID
	}
	var admins []account.AdminStanding
	var now time.Time
	if g != nil {
		var err error
		if admins, now, err = g.lock(ctx); err != nil {
			return nil, err
		}
	} else {
		now = clock.Now()
	}
	a, err := accounts.FindByID(ctx, account.ID(id))
	if err != nil {
		return nil, err
	}
	if err := fn(a, now); err != nil {
		return nil, err
	}
	if g != nil && (!a.IsActive() || !a.HasRole(g.role)) {
		if err := account.GuardLastAdmin(admins, a.ID()); err != nil {
			return nil, err
		}
	}
	if err := accounts.Save(ctx, a); err != nil {
		return nil, fmt.Errorf("save account %s: %w", id, err)
	}

	return mapper.ToAccountOutput(a), nil
}
