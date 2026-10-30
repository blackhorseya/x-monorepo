package memory

import (
	"context"
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
