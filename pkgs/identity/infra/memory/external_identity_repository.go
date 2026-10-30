package memory

import (
	"cmp"
	"context"
	"slices"
	"sync"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/externalidentity"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

var _ externalidentity.Repository = (*ExternalIdentityRepository)(nil)

type ExternalIdentityRepository struct {
	mu   sync.RWMutex
	data map[[2]string]*externalidentity.Identity
}

func NewExternalIdentityRepository() *ExternalIdentityRepository {
	return &ExternalIdentityRepository{data: make(map[[2]string]*externalidentity.Identity)}
}

func (i *ExternalIdentityRepository) Find(c context.Context, issuer, subject string) (*externalidentity.Identity, error) {
	if err := contextx.FromContext(c).Err(); err != nil {
		return nil, err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()
	e, ok := i.data[[2]string{issuer, subject}]
	if !ok {
		return nil, externalidentity.ErrNotFound
	}

	return e, nil
}

func (i *ExternalIdentityRepository) Add(c context.Context, e *externalidentity.Identity) error {
	if err := contextx.FromContext(c).Err(); err != nil {
		return err
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	k := [2]string{e.Issuer(), e.Subject()}
	if _, exists := i.data[k]; exists {
		return externalidentity.ErrDuplicate
	}
	i.data[k] = e

	return nil
}

func (i *ExternalIdentityRepository) ListByAccount(c context.Context, accountID string) ([]*externalidentity.Identity, error) {
	if err := contextx.FromContext(c).Err(); err != nil {
		return nil, err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make([]*externalidentity.Identity, 0)
	for _, e := range i.data {
		if e.AccountID() == accountID {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b *externalidentity.Identity) int {
		return cmp.Or(b.CreatedAt().Compare(a.CreatedAt()), cmp.Compare(a.Issuer(), b.Issuer()), cmp.Compare(a.Subject(), b.Subject()))
	})

	return out, nil
}
