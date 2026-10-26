package postgres

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/apikey"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

var _ apikey.Repository = (*APIKeyRepository)(nil)

// APIKeyRepository persists APIKey in identity.api_keys with optimistic locking.
type APIKeyRepository struct {
	db *gorm.DB
}

func NewAPIKeyRepository(db *gorm.DB) *APIKeyRepository { return &APIKeyRepository{db: db} }

// apiKeyRow mirrors columns, not the aggregate. Nullable timestamps are
// pointers: NULL is the domain's zero time.
type apiKeyRow struct {
	ID        string     `gorm:"primaryKey"`
	AccountID string     `gorm:"not null"`
	Name      string     `gorm:"not null"`
	Prefix    string     `gorm:"not null"`
	Hash      []byte     `gorm:"not null"`
	Status    string     `gorm:"not null"`
	Version   int64      `gorm:"not null"`
	CreatedAt time.Time  `gorm:"not null;autoCreateTime:false"`
	ExpiresAt *time.Time `gorm:"autoUpdateTime:false"`
	RevokedAt *time.Time `gorm:"autoUpdateTime:false"`
}

func (apiKeyRow) TableName() string { return Schema + ".api_keys" }

func toRow(k *apikey.APIKey) apiKeyRow {
	h := k.Hash()
	row := apiKeyRow{
		ID: k.ID().String(), AccountID: k.AccountID(), Name: k.Name(), Prefix: k.Prefix(), Hash: h[:],
		Status: k.Status().String(), Version: k.Version(), CreatedAt: k.CreatedAt(),
	}
	if !k.ExpiresAt().IsZero() {
		t := k.ExpiresAt()
		row.ExpiresAt = &t
	}
	if !k.RevokedAt().IsZero() {
		t := k.RevokedAt()
		row.RevokedAt = &t
	}

	return row
}

func toAPIKey(row apiKeyRow) (*apikey.APIKey, error) {
	status, err := apikey.ParseStatus(row.Status)
	if err != nil {
		return nil, fmt.Errorf("api key %s: %w", row.ID, err)
	}
	var h apikey.Hash
	copy(h[:], row.Hash)
	p := apikey.ReconstituteParams{
		ID: apikey.ID(row.ID), AccountID: row.AccountID, Name: row.Name, Prefix: row.Prefix, Hash: h,
		Status: status, Version: row.Version, CreatedAt: row.CreatedAt.UTC(),
	}
	if row.ExpiresAt != nil {
		p.ExpiresAt = row.ExpiresAt.UTC()
	}
	if row.RevokedAt != nil {
		p.RevokedAt = row.RevokedAt.UTC()
	}

	return apikey.Reconstitute(p)
}

func (i *APIKeyRepository) find(c context.Context, what string, query string, arg any) (*apikey.APIKey, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var row apiKeyRow
	if err := platformpg.TxOrDB(ctx, i.db).Take(&row, query, arg).Error; err != nil {
		return nil, translate(fmt.Errorf("find api key by %s: %w", what, err), apikey.ErrNotFound, nil)
	}

	return toAPIKey(row)
}

func (i *APIKeyRepository) FindByID(c context.Context, id apikey.ID) (*apikey.APIKey, error) {
	return i.find(c, "id", "id = ?", id.String())
}

func (i *APIKeyRepository) FindByHash(c context.Context, h apikey.Hash) (*apikey.APIKey, error) {
	return i.find(c, "hash", "hash = ?", h[:])
}

func (i *APIKeyRepository) Add(c context.Context, k *apikey.APIKey) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	row := toRow(k)
	row.Version = 1
	if err := platformpg.TxOrDB(ctx, i.db).Create(&row).Error; err != nil {
		return translate(fmt.Errorf("insert api key %s: %w", k.ID(), err), nil, apikey.ErrDuplicate)
	}

	return nil
}

// Save updates `WHERE version = <loaded>`, bumping it; zero rows matched is ErrStale.
func (i *APIKeyRepository) Save(c context.Context, k *apikey.APIKey) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	row := toRow(k)
	res := platformpg.TxOrDB(ctx, i.db).Model(&apiKeyRow{}).
		Where("id = ? AND version = ?", row.ID, k.Version()).
		Updates(map[string]any{
			"name":       row.Name,
			"status":     row.Status,
			"expires_at": row.ExpiresAt,
			"revoked_at": row.RevokedAt,
			"version":    gorm.Expr("version + 1"),
		})
	if res.Error != nil {
		return fmt.Errorf("update api key %s: %w", k.ID(), res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("update api key %s: %w", k.ID(), apikey.ErrStale)
	}

	return nil
}

func (i *APIKeyRepository) ListByAccount(c context.Context, accountID string) ([]*apikey.APIKey, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var rows []apiKeyRow
	if err := platformpg.TxOrDB(ctx, i.db).Where("account_id = ?", accountID).Order("created_at DESC, id DESC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list api keys of %s: %w", accountID, err)
	}
	out := make([]*apikey.APIKey, 0, len(rows))
	for _, row := range rows {
		k, err := toAPIKey(row)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}

	return out, nil
}
