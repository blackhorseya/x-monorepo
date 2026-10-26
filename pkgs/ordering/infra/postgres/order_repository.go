package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

var _ order.Repository = (*OrderRepository)(nil)

// OrderRepository persists the Order aggregate across orders + order_items.
type OrderRepository struct {
	db *gorm.DB
}

func NewOrderRepository(db *gorm.DB) *OrderRepository {
	return &OrderRepository{db: db}
}

// orderRow / orderItemRow are persistence models: they mirror columns, not the
// aggregate. Timestamps come from the domain (port.Clock), so gorm's automatic
// created_at/updated_at tracking is switched off.
type orderRow struct {
	ID         string         `gorm:"primaryKey"`
	CustomerID string         `gorm:"not null"`
	Status     string         `gorm:"not null"`
	CreatedAt  time.Time      `gorm:"not null;autoCreateTime:false"`
	UpdatedAt  time.Time      `gorm:"not null;autoUpdateTime:false"`
	Version    int64          `gorm:"not null"`
	Items      []orderItemRow `gorm:"foreignKey:OrderID;references:ID"`
}

func (orderRow) TableName() string { return Schema + ".orders" }

type orderItemRow struct {
	OrderID        string `gorm:"primaryKey"`
	Position       int    `gorm:"primaryKey"`
	SKU            string `gorm:"not null"`
	Quantity       int32  `gorm:"not null"`
	UnitPriceMinor int64  `gorm:"not null"`
	Currency       string `gorm:"not null"`
}

func (orderItemRow) TableName() string { return Schema + ".order_items" }

func (i *OrderRepository) FindByID(c context.Context, id order.ID) (*order.Order, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var row orderRow
	err := platformpg.TxOrDB(ctx, i.db).
		Preload("Items", func(db *gorm.DB) *gorm.DB { return db.Order("position") }).
		Take(&row, "id = ?", string(id)).Error
	if err != nil {
		return nil, translate(fmt.Errorf("find order %s: %w", id, err))
	}

	return toDomain(row)
}

// Save writes the whole aggregate in one transaction. A never-saved order
// (Version 0) is a plain INSERT, so an id collision surfaces as ErrDuplicate
// instead of silently overwriting. A loaded order is an UPDATE guarded by
// `version = <loaded>` that bumps the version; zero rows means another writer
// saved first → ErrStale. Items are replaced (delete + insert keeps positions
// authoritative).
func (i *OrderRepository) Save(c context.Context, o *order.Order) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	row := toRow(o)
	// Joins the use case's transaction when there is one (Transactor), else opens its own.
	err := platformpg.Transaction(ctx, i.db, func(_ context.Context, tx *gorm.DB) error {
		if o.Version() == 0 {
			row.Version = 1
			if err := tx.Omit(clause.Associations).Create(&row).Error; err != nil {
				return fmt.Errorf("insert order %s: %w", o.ID(), err)
			}
		} else {
			res := tx.Model(&orderRow{}).
				Where("id = ? AND version = ?", row.ID, o.Version()).
				Updates(map[string]any{
					"customer_id": row.CustomerID,
					"status":      row.Status,
					"updated_at":  row.UpdatedAt,
					"version":     gorm.Expr("version + 1"),
				})
			if res.Error != nil {
				return fmt.Errorf("update order %s: %w", o.ID(), res.Error)
			}
			if res.RowsAffected == 0 {
				return fmt.Errorf("update order %s: %w", o.ID(), order.ErrStale)
			}
			if err := tx.Where("order_id = ?", row.ID).Delete(&orderItemRow{}).Error; err != nil {
				return fmt.Errorf("clear order items %s: %w", o.ID(), err)
			}
		}
		if err := tx.Create(&row.Items).Error; err != nil {
			return fmt.Errorf("insert order items %s: %w", o.ID(), err)
		}
		return nil
	})

	return translate(err)
}

// List is a keyset query: the row-value comparison matches the composite
// indexes added in migration 0002, so every page is an index range scan. One
// extra row is fetched to know whether a next page exists without a COUNT.
func (i *OrderRepository) List(c context.Context, p order.ListParams) ([]*order.Order, *order.Cursor, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	q := platformpg.TxOrDB(ctx, i.db).
		Preload("Items", func(db *gorm.DB) *gorm.DB { return db.Order("position") }).
		Order("created_at DESC, id DESC")
	if p.CustomerID != "" {
		q = q.Where("customer_id = ?", p.CustomerID)
	}
	if p.Status != nil {
		q = q.Where("status = ?", p.Status.String())
	}
	if p.After != nil {
		q = q.Where("(created_at, id) < (?, ?)", p.After.CreatedAt(), p.After.ID().String())
	}
	if p.Limit > 0 {
		q = q.Limit(p.Limit + 1)
	}

	var rows []orderRow
	if err := q.Find(&rows).Error; err != nil {
		return nil, nil, translate(fmt.Errorf("list orders: %w", err))
	}

	hasMore := p.Limit > 0 && len(rows) > p.Limit
	if hasMore {
		rows = rows[:p.Limit]
	}

	out := make([]*order.Order, 0, len(rows))
	for _, row := range rows {
		o, err := toDomain(row)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, o)
	}

	var next *order.Cursor
	if hasMore {
		cur := order.CursorOf(out[len(out)-1])
		next = &cur
	}

	return out, next, nil
}

func toRow(o *order.Order) orderRow {
	items := o.Items()
	row := orderRow{
		ID:         string(o.ID()),
		CustomerID: o.CustomerID(),
		Status:     o.Status().String(),
		CreatedAt:  o.CreatedAt(),
		UpdatedAt:  o.UpdatedAt(),
		Version:    o.Version(),
		Items:      make([]orderItemRow, 0, len(items)),
	}
	for pos, it := range items {
		row.Items = append(row.Items, orderItemRow{
			OrderID:        row.ID,
			Position:       pos,
			SKU:            it.SKU(),
			Quantity:       it.Quantity(),
			UnitPriceMinor: it.UnitPrice().Amount(),
			Currency:       it.UnitPrice().Currency(),
		})
	}

	return row
}

func toDomain(row orderRow) (*order.Order, error) {
	items := make([]order.LineItem, 0, len(row.Items))
	for _, r := range row.Items {
		price, err := kernel.NewMoney(r.UnitPriceMinor, r.Currency)
		if err != nil {
			return nil, fmt.Errorf("load order %s item %s: %w", row.ID, r.SKU, err)
		}
		it, err := order.NewLineItem(r.SKU, r.Quantity, price)
		if err != nil {
			return nil, fmt.Errorf("load order %s item %s: %w", row.ID, r.SKU, err)
		}
		items = append(items, it)
	}

	status, err := order.ParseStatus(row.Status)
	if err != nil {
		return nil, fmt.Errorf("load order %s: %w", row.ID, err)
	}

	return order.Reconstitute(order.ReconstituteParams{
		ID:         order.ID(row.ID),
		CustomerID: row.CustomerID,
		Items:      items,
		Status:     status,
		CreatedAt:  row.CreatedAt.UTC(),
		UpdatedAt:  row.UpdatedAt.UTC(),
		Version:    row.Version,
	})
}

// translate maps driver errors onto domain errors so callers only ever see
// order.Err* (checked with errors.Is) or context errors. Anything else is an
// infrastructure failure and stays wrapped for logging. gorm's TranslateError
// (see platform/postgres.NewDB) has already turned SQLSTATE 23505 into
// gorm.ErrDuplicatedKey.
func translate(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gorm.ErrRecordNotFound):
		return order.ErrNotFound
	case errors.Is(err, gorm.ErrDuplicatedKey):
		return order.ErrDuplicate
	default:
		return err
	}
}

// CountByStatus is one GROUP BY over the orders table; statuses with no row are absent.
func (i *OrderRepository) CountByStatus(c context.Context) ([]order.StatusCount, error) {
	ctx := contextx.FromContext(c)

	var rows []struct {
		Status string
		N      int64
		Oldest time.Time
	}
	err := platformpg.TxOrDB(ctx, i.db).
		Raw(`SELECT status, count(*) AS n, min(updated_at) AS oldest FROM ` + Schema + `.orders GROUP BY status ORDER BY status`).
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("count orders by status: %w", err)
	}

	out := make([]order.StatusCount, 0, len(rows))
	for _, r := range rows {
		st, err := order.ParseStatus(r.Status)
		if err != nil {
			return nil, err
		}
		out = append(out, order.NewStatusCount(st, r.N, r.Oldest))
	}

	return out, nil
}
