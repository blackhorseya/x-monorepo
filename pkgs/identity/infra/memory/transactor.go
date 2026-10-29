package memory

import (
	"context"
	"sync"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/port"
)

var _ port.Transactor = (*Transactor)(nil)

// Transactor is the no-database Transactor: there is nothing to roll back, but
// writers still run one at a time, which is what Postgres gets from locking
// the admin rows in AdminCensus. Do is not reentrant: fn must not call Do.
type Transactor struct {
	mu sync.Mutex
}

func NewTransactor() *Transactor { return &Transactor{} }

func (i *Transactor) Do(c context.Context, fn func(c context.Context) error) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	return fn(c)
}
