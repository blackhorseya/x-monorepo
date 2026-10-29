package postgres

import (
	"context"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/catalog/application/port"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

var _ port.Transactor = (*Transactor)(nil)

// Transactor opens one gorm transaction for fn and carries it in the context,
// where the repositories and the eventbus publisher pick it up.
type Transactor struct {
	db *gorm.DB
}

func NewTransactor(db *gorm.DB) *Transactor { return &Transactor{db: db} }

func (i *Transactor) Do(c context.Context, fn func(c context.Context) error) error {
	return platformpg.Transaction(c, i.db, func(c context.Context, _ *gorm.DB) error { return fn(c) })
}
