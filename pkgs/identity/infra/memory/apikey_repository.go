package memory

import (
	"context"
	"sort"
	"sync"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/apikey"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

var _ apikey.Repository = (*APIKeyRepository)(nil)

type APIKeyRepository struct {
	mu     sync.RWMutex
	data   map[apikey.ID]*apikey.APIKey
	byHash map[apikey.Hash]apikey.ID
}

func NewAPIKeyRepository() *APIKeyRepository {
	return &APIKeyRepository{data: make(map[apikey.ID]*apikey.APIKey), byHash: make(map[apikey.Hash]apikey.ID)}
}

func (i *APIKeyRepository) FindByID(c context.Context, id apikey.ID) (*apikey.APIKey, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	k, ok := i.data[id]
	if !ok {
		return nil, apikey.ErrNotFound
	}

	return cloneKey(k, k.Version())
}

func (i *APIKeyRepository) FindByHash(c context.Context, h apikey.Hash) (*apikey.APIKey, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	id, ok := i.byHash[h]
	if !ok {
		return nil, apikey.ErrNotFound
	}
	k := i.data[id]

	return cloneKey(k, k.Version())
}

func (i *APIKeyRepository) Add(c context.Context, k *apikey.APIKey) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	if _, exists := i.data[k.ID()]; exists {
		return apikey.ErrDuplicate
	}
	if _, exists := i.byHash[k.Hash()]; exists {
		return apikey.ErrDuplicate
	}
	cp, err := cloneKey(k, 1)
	if err != nil {
		return err
	}
	i.data[k.ID()] = cp
	i.byHash[k.Hash()] = k.ID()

	return nil
}

func (i *APIKeyRepository) Save(c context.Context, k *apikey.APIKey) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	cur, exists := i.data[k.ID()]
	if !exists || cur.Version() != k.Version() {
		return apikey.ErrStale
	}
	cp, err := cloneKey(k, k.Version()+1)
	if err != nil {
		return err
	}
	i.data[k.ID()] = cp

	return nil
}

func (i *APIKeyRepository) ListByAccount(c context.Context, accountID string) ([]*apikey.APIKey, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	out := make([]*apikey.APIKey, 0)
	for _, k := range i.data {
		if k.AccountID() != accountID {
			continue
		}
		cp, err := cloneKey(k, k.Version())
		if err != nil {
			return nil, err
		}
		out = append(out, cp)
	}
	// Newest first, id desc as the tie-break: the Postgres ORDER BY.
	sort.Slice(out, func(a, b int) bool {
		if !out[a].CreatedAt().Equal(out[b].CreatedAt()) {
			return out[a].CreatedAt().After(out[b].CreatedAt())
		}

		return out[a].ID() > out[b].ID()
	})

	return out, nil
}

func cloneKey(k *apikey.APIKey, version int64) (*apikey.APIKey, error) {
	return apikey.Reconstitute(apikey.ReconstituteParams{
		ID: k.ID(), AccountID: k.AccountID(), Name: k.Name(), Prefix: k.Prefix(), Hash: k.Hash(),
		Status: k.Status(), Version: version, CreatedAt: k.CreatedAt(), ExpiresAt: k.ExpiresAt(), RevokedAt: k.RevokedAt(),
	})
}
