package postgres

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

var _ account.Repository = (*AccountRepository)(nil)

// AccountRepository persists Account in identity.accounts with optimistic locking.
type AccountRepository struct {
	db *gorm.DB
}

func NewAccountRepository(db *gorm.DB) *AccountRepository { return &AccountRepository{db: db} }

// roles is the jsonb column: a JSON array of strings.
type roles []string

func (r roles) Value() (driver.Value, error) {
	if r == nil {
		r = roles{}
	}
	b, err := json.Marshal([]string(r))

	return string(b), err
}

func (r *roles) Scan(src any) error {
	var b []byte
	switch v := src.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	case nil:
		*r = roles{}
		return nil
	default:
		return fmt.Errorf("roles: cannot scan %T", src)
	}

	return json.Unmarshal(b, (*[]string)(r))
}

// accountRow mirrors columns, not the aggregate. Timestamps come from the
// domain (port.Clock), so gorm's automatic tracking is switched off.
type accountRow struct {
	ID        string    `gorm:"primaryKey"`
	Name      string    `gorm:"not null"`
	Roles     roles     `gorm:"type:jsonb;not null"`
	Status    string    `gorm:"not null"`
	Version   int64     `gorm:"not null"`
	CreatedAt time.Time `gorm:"not null;autoCreateTime:false"`
	UpdatedAt time.Time `gorm:"not null;autoUpdateTime:false"`
	// SessionsRevokedAt NULL: never revoked.
	SessionsRevokedAt *time.Time `gorm:"autoUpdateTime:false"`
}

func (accountRow) TableName() string { return Schema + ".accounts" }

func toAccount(row accountRow) (*account.Account, error) {
	status, err := account.ParseStatus(row.Status)
	if err != nil {
		return nil, fmt.Errorf("account %s: %w", row.ID, err)
	}

	var revoked time.Time
	if row.SessionsRevokedAt != nil {
		revoked = row.SessionsRevokedAt.UTC()
	}

	return account.Reconstitute(account.ReconstituteParams{
		ID: account.ID(row.ID), Name: row.Name, Roles: row.Roles, Status: status,
		Version: row.Version, CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(),
		SessionsRevokedAt: revoked,
	})
}

func (i *AccountRepository) FindByID(c context.Context, id account.ID) (*account.Account, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var row accountRow
	if err := platformpg.TxOrDB(ctx, i.db).Take(&row, "id = ?", id.String()).Error; err != nil {
		return nil, translate(fmt.Errorf("find account %s: %w", id, err), account.ErrNotFound, nil)
	}

	return toAccount(row)
}

func (i *AccountRepository) Add(c context.Context, a *account.Account) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	row := accountRow{ID: a.ID().String(), Name: a.Name(), Roles: a.Roles(), Status: a.Status().String(), Version: 1, CreatedAt: a.CreatedAt(), UpdatedAt: a.UpdatedAt(), SessionsRevokedAt: nullTime(a.SessionsRevokedAt())}
	if err := platformpg.TxOrDB(ctx, i.db).Create(&row).Error; err != nil {
		return translate(fmt.Errorf("insert account %s: %w", a.ID(), err), nil, account.ErrDuplicate)
	}

	return nil
}

// Save updates `WHERE version = <loaded>`, bumping it; zero rows matched is
// ErrStale (the caller reloads and retries).
func (i *AccountRepository) Save(c context.Context, a *account.Account) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	res := platformpg.TxOrDB(ctx, i.db).Model(&accountRow{}).
		Where("id = ? AND version = ?", a.ID().String(), a.Version()).
		Updates(map[string]any{
			"name":                a.Name(),
			"roles":               roles(a.Roles()),
			"status":              a.Status().String(),
			"updated_at":          a.UpdatedAt(),
			"sessions_revoked_at": nullTime(a.SessionsRevokedAt()),
			"version":             gorm.Expr("version + 1"),
		})
	if res.Error != nil {
		return fmt.Errorf("update account %s: %w", a.ID(), res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("update account %s: %w", a.ID(), account.ErrStale)
	}

	return nil
}

func (i *AccountRepository) List(c context.Context, p account.ListParams) ([]*account.Account, account.ID, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	q := platformpg.TxOrDB(ctx, i.db).Order("id ASC")
	if p.After != "" {
		q = q.Where("id > ?", p.After.String())
	}
	if p.Limit > 0 {
		q = q.Limit(p.Limit + 1)
	}
	var rows []accountRow
	if err := q.Find(&rows).Error; err != nil {
		return nil, "", fmt.Errorf("list accounts: %w", err)
	}
	hasMore := p.Limit > 0 && len(rows) > p.Limit
	if hasMore {
		rows = rows[:p.Limit]
	}
	out := make([]*account.Account, 0, len(rows))
	for _, row := range rows {
		a, err := toAccount(row)
		if err != nil {
			return nil, "", err
		}
		out = append(out, a)
	}
	var next account.ID
	if hasMore {
		next = out[len(out)-1].ID()
	}

	return out, next, nil
}

// nullTime maps the domain's zero time to NULL.
func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}

	return &t
}
