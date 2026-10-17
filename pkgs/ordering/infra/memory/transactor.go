package memory

import (
	"context"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/port"
)

var _ port.Transactor = Transactor{}

// Transactor is the no-database Transactor: there is nothing to roll back,
// fn just runs.
type Transactor struct{}

func NewTransactor() Transactor { return Transactor{} }

func (Transactor) Do(c context.Context, fn func(c context.Context) error) error { return fn(c) }
