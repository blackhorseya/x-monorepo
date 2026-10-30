package postgres

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

var _ port.AuditLog = (*AuditLog)(nil)

// AuditLog appends to identity.audit_events inside the caller's transaction.
type AuditLog struct {
	db *gorm.DB
}

func NewAuditLog(db *gorm.DB) *AuditLog { return &AuditLog{db: db} }

type auditRow struct {
	ID              int64     `gorm:"primaryKey;autoIncrement"`
	Actor           string    `gorm:"not null"`
	Action          string    `gorm:"not null"`
	TargetAccountID string    `gorm:"not null"`
	OccurredAt      time.Time `gorm:"not null;autoCreateTime:false"`
}

func (auditRow) TableName() string { return Schema + ".audit_events" }

func (i *AuditLog) Record(c context.Context, e dto.AuditEvent) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	row := auditRow{Actor: e.Actor, Action: e.Action, TargetAccountID: e.TargetAccountID, OccurredAt: e.At}
	if err := platformpg.TxOrDB(ctx, i.db).Create(&row).Error; err != nil {
		return fmt.Errorf("record audit %s: %w", e.Action, err)
	}

	return nil
}
