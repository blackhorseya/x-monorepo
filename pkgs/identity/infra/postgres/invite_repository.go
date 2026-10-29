package postgres

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/invite"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

var _ invite.Repository = (*InviteRepository)(nil)

// InviteRepository persists Invite in identity.invites with optimistic locking.
type InviteRepository struct {
	db *gorm.DB
}

func NewInviteRepository(db *gorm.DB) *InviteRepository { return &InviteRepository{db: db} }

type inviteRow struct {
	ID        string     `gorm:"primaryKey"`
	Email     string     `gorm:"not null"`
	AccountID string     `gorm:"not null"`
	Version   int64      `gorm:"not null"`
	CreatedAt time.Time  `gorm:"not null;autoCreateTime:false"`
	ExpiresAt time.Time  `gorm:"not null;autoCreateTime:false"`
	ClaimedAt *time.Time `gorm:"autoUpdateTime:false"`
}

func (inviteRow) TableName() string { return Schema + ".invites" }

func toInvite(row inviteRow) (*invite.Invite, error) {
	var claimed time.Time
	if row.ClaimedAt != nil {
		claimed = row.ClaimedAt.UTC()
	}

	return invite.Reconstitute(invite.ReconstituteParams{
		ID: invite.ID(row.ID), Email: row.Email, AccountID: row.AccountID, Version: row.Version,
		CreatedAt: row.CreatedAt.UTC(), ExpiresAt: row.ExpiresAt.UTC(), ClaimedAt: claimed,
	})
}

func (i *InviteRepository) Add(c context.Context, v *invite.Invite) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	row := inviteRow{ID: v.ID().String(), Email: v.Email(), AccountID: v.AccountID(), Version: 1, CreatedAt: v.CreatedAt(), ExpiresAt: v.ExpiresAt(), ClaimedAt: nullTime(v.ClaimedAt())}
	if err := platformpg.TxOrDB(ctx, i.db).Create(&row).Error; err != nil {
		return fmt.Errorf("insert invite %s: %w", v.ID(), err)
	}

	return nil
}

// FindLatestByEmail locks the row (FOR UPDATE): a second claimant of the same
// email waits here until the first commits, then sees the invite spent.
func (i *InviteRepository) FindLatestByEmail(c context.Context, email string) (*invite.Invite, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var row inviteRow
	if err := platformpg.TxOrDB(ctx, i.db).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("email = ?", email).Order("created_at DESC, id DESC").Limit(1).Take(&row).Error; err != nil {
		return nil, translate(fmt.Errorf("find invite for email: %w", err), invite.ErrNotFound, nil)
	}

	return toInvite(row)
}

func (i *InviteRepository) Save(c context.Context, v *invite.Invite) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	res := platformpg.TxOrDB(ctx, i.db).Model(&inviteRow{}).
		Where("id = ? AND version = ?", v.ID().String(), v.Version()).
		Updates(map[string]any{"claimed_at": nullTime(v.ClaimedAt()), "version": gorm.Expr("version + 1")})
	if res.Error != nil {
		return fmt.Errorf("update invite %s: %w", v.ID(), res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("update invite %s: %w", v.ID(), invite.ErrStale)
	}

	return nil
}
