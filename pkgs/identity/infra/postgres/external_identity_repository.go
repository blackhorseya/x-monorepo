package postgres

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/externalidentity"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

var _ externalidentity.Repository = (*ExternalIdentityRepository)(nil)

// ExternalIdentityRepository persists bindings in identity.external_identities;
// the (issuer, subject) primary key is what makes a binding one-to-one.
type ExternalIdentityRepository struct {
	db *gorm.DB
}

func NewExternalIdentityRepository(db *gorm.DB) *ExternalIdentityRepository {
	return &ExternalIdentityRepository{db: db}
}

type externalIdentityRow struct {
	Issuer    string    `gorm:"primaryKey"`
	Subject   string    `gorm:"primaryKey"`
	AccountID string    `gorm:"not null"`
	CreatedAt time.Time `gorm:"not null;autoCreateTime:false"`
}

func (externalIdentityRow) TableName() string { return Schema + ".external_identities" }

func (i *ExternalIdentityRepository) Find(c context.Context, issuer, subject string) (*externalidentity.Identity, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var row externalIdentityRow
	if err := platformpg.TxOrDB(ctx, i.db).Take(&row, "issuer = ? AND subject = ?", issuer, subject).Error; err != nil {
		return nil, translate(fmt.Errorf("find external identity: %w", err), externalidentity.ErrNotFound, nil)
	}

	return externalidentity.New(row.Issuer, row.Subject, row.AccountID, row.CreatedAt.UTC())
}

func (i *ExternalIdentityRepository) Add(c context.Context, e *externalidentity.Identity) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	row := externalIdentityRow{Issuer: e.Issuer(), Subject: e.Subject(), AccountID: e.AccountID(), CreatedAt: e.CreatedAt()}
	if err := platformpg.TxOrDB(ctx, i.db).Create(&row).Error; err != nil {
		return translate(fmt.Errorf("insert external identity: %w", err), nil, externalidentity.ErrDuplicate)
	}

	return nil
}

func (i *ExternalIdentityRepository) ListByAccount(c context.Context, accountID string) ([]*externalidentity.Identity, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var rows []externalIdentityRow
	if err := platformpg.TxOrDB(ctx, i.db).Where("account_id = ?", accountID).
		Order("created_at DESC, issuer ASC, subject ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list external identities of account %s: %w", accountID, err)
	}
	out := make([]*externalidentity.Identity, 0, len(rows))
	for _, row := range rows {
		e, err := externalidentity.New(row.Issuer, row.Subject, row.AccountID, row.CreatedAt.UTC())
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}

	return out, nil
}
