// Package memory holds in-memory repositories for local dev and tests. They
// store copies so callers can't mutate stored state by aliasing, and they
// honour the same contracts as infra/postgres (ErrStale, ErrDuplicate).
package memory

import (
	"context"
	"slices"
	"sync"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

var _ account.Repository = (*AccountRepository)(nil)

type AccountRepository struct {
	mu   sync.RWMutex
	data map[account.ID]*account.Account
}

func NewAccountRepository() *AccountRepository {
	return &AccountRepository{data: make(map[account.ID]*account.Account)}
}

func (i *AccountRepository) FindByID(c context.Context, id account.ID) (*account.Account, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	a, ok := i.data[id]
	if !ok {
		return nil, account.ErrNotFound
	}

	return cloneAccount(a, a.Version())
}

func (i *AccountRepository) Add(c context.Context, a *account.Account) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	if _, exists := i.data[a.ID()]; exists {
		return account.ErrDuplicate
	}
	cp, err := cloneAccount(a, 1)
	if err != nil {
		return err
	}
	i.data[a.ID()] = cp

	return nil
}

// Save follows the Postgres contract: the stored version must still match
// (ErrStale otherwise), and advances by one.
func (i *AccountRepository) Save(c context.Context, a *account.Account) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	cur, exists := i.data[a.ID()]
	if !exists || cur.Version() != a.Version() {
		return account.ErrStale
	}
	cp, err := cloneAccount(a, a.Version()+1)
	if err != nil {
		return err
	}
	i.data[a.ID()] = cp

	return nil
}

func (i *AccountRepository) List(c context.Context, p account.ListParams) ([]*account.Account, account.ID, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	ids := make([]account.ID, 0, len(i.data))
	for id := range i.data {
		if id > p.After {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	var next account.ID
	if p.Limit > 0 && len(ids) > p.Limit {
		ids = ids[:p.Limit]
		next = ids[p.Limit-1]
	}
	out := make([]*account.Account, 0, len(ids))
	for _, id := range ids {
		cp, err := cloneAccount(i.data[id], i.data[id].Version())
		if err != nil {
			return nil, "", err
		}
		out = append(out, cp)
	}

	return out, next, nil
}

func cloneAccount(a *account.Account, version int64) (*account.Account, error) {
	return account.Reconstitute(account.ReconstituteParams{
		ID: a.ID(), Name: a.Name(), Roles: a.Roles(), Status: a.Status(),
		Version: version, CreatedAt: a.CreatedAt(), UpdatedAt: a.UpdatedAt(), SessionsRevokedAt: a.SessionsRevokedAt(),
	})
}
