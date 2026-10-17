package postgres

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

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

// Save is one upsert: insert the row, or update it only if nobody else did
// since it was loaded (`WHERE stocks.version = EXCLUDED.version`), bumping the
// version. Zero rows affected means the check failed → ErrStale.
func (i *StockRepository) Save(c context.Context, s *stock.Stock) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	row := stockRow{SKU: s.SKU().String(), OnHand: s.OnHand(), Reserved: s.Reserved(), Version: s.Version(), UpdatedAt: s.UpdatedAt()}
	res := platformpg.TxOrDB(ctx, i.db).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "sku"}},
		DoUpdates: clause.Set{
			{Column: clause.Column{Name: "on_hand"}, Value: row.OnHand},
			{Column: clause.Column{Name: "reserved"}, Value: row.Reserved},
			{Column: clause.Column{Name: "updated_at"}, Value: row.UpdatedAt},
			{Column: clause.Column{Name: "version"}, Value: gorm.Expr("stocks.version + 1")},
		},
		Where: clause.Where{Exprs: []clause.Expression{gorm.Expr("stocks.version = EXCLUDED.version")}},
	}).Create(&row)
	if res.Error != nil {
		return fmt.Errorf("save stock %s: %w", s.SKU(), res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("save stock %s: %w", s.SKU(), stock.ErrStale)
	}

	return nil
}
