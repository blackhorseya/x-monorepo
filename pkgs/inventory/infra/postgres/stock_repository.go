package postgres

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/stock"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

var _ stock.Repository = (*StockRepository)(nil)

// StockRepository persists Stock in inventory.stocks with optimistic locking.
type StockRepository struct {
	db *gorm.DB
}

func NewStockRepository(db *gorm.DB) *StockRepository { return &StockRepository{db: db} }

// stockRow mirrors columns, not the aggregate. Timestamps come from the
// domain (port.Clock), so gorm's automatic tracking is switched off.
type stockRow struct {
	SKU       string    `gorm:"primaryKey"`
	OnHand    int32     `gorm:"not null"`
	Reserved  int32     `gorm:"not null"`
	Version   int64     `gorm:"not null"`
	UpdatedAt time.Time `gorm:"not null;autoUpdateTime:false"`
}

func (stockRow) TableName() string { return Schema + ".stocks" }

func (i *StockRepository) FindBySKU(c context.Context, sku stock.SKU) (*stock.Stock, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var row stockRow
	err := platformpg.TxOrDB(ctx, i.db).Take(&row, "sku = ?", sku.String()).Error
	if err != nil {
		return nil, translate(fmt.Errorf("find stock %s: %w", sku, err), stock.ErrNotFound, nil)
	}

	return stock.Reconstitute(stock.ReconstituteParams{
		SKU:       stock.SKU(row.SKU),
		OnHand:    row.OnHand,
		Reserved:  row.Reserved,
		Version:   row.Version,
		UpdatedAt: row.UpdatedAt.UTC(),
	})
}

// Save inserts a never-saved stock (Version 0) with version 1, or updates a
// loaded one `WHERE version = <loaded>`, bumping it. Both ways a lost race is
// ErrStale: a second insert of the same SKU hits the primary key, an update
// whose version moved on matches zero rows. Either way the caller reloads and
// retries (SetStock then restocks the row the other writer created).
func (i *StockRepository) Save(c context.Context, s *stock.Stock) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	db := platformpg.TxOrDB(ctx, i.db)
	if s.Version() == 0 {
		row := stockRow{SKU: s.SKU().String(), OnHand: s.OnHand(), Reserved: s.Reserved(), Version: 1, UpdatedAt: s.UpdatedAt()}
		if err := db.Create(&row).Error; err != nil {
			return translate(fmt.Errorf("insert stock %s: %w", s.SKU(), err), nil, fmt.Errorf("insert stock %s: %w", s.SKU(), stock.ErrStale))
		}

		return nil
	}

	res := db.Model(&stockRow{}).
		Where("sku = ? AND version = ?", s.SKU().String(), s.Version()).
		Updates(map[string]any{
			"on_hand":    s.OnHand(),
			"reserved":   s.Reserved(),
			"updated_at": s.UpdatedAt(),
			"version":    gorm.Expr("version + 1"),
		})
	if res.Error != nil {
		return fmt.Errorf("update stock %s: %w", s.SKU(), res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("update stock %s: %w", s.SKU(), stock.ErrStale)
	}

	return nil
}
