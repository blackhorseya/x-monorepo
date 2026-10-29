package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/catalog/domain/product"
	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

var _ product.Repository = (*ProductRepository)(nil)

// ProductRepository persists Product in catalog.products with optimistic locking.
type ProductRepository struct {
	db *gorm.DB
}

func NewProductRepository(db *gorm.DB) *ProductRepository { return &ProductRepository{db: db} }

// productRow mirrors columns, not the aggregate. Timestamps come from the
// domain (port.Clock), so gorm's automatic tracking is switched off.
type productRow struct {
	SKU           string    `gorm:"primaryKey"`
	Name          string    `gorm:"not null"`
	PriceAmount   int64     `gorm:"not null"`
	PriceCurrency string    `gorm:"not null"`
	Active        bool      `gorm:"not null"`
	Version       int64     `gorm:"not null"`
	UpdatedAt     time.Time `gorm:"not null;autoUpdateTime:false"`
}

func (productRow) TableName() string { return Schema + ".products" }

func (r productRow) toAggregate() (*product.Product, error) {
	price, err := kernel.NewMoney(r.PriceAmount, r.PriceCurrency)
	if err != nil {
		return nil, fmt.Errorf("product %s: %w", r.SKU, err)
	}

	return product.Reconstitute(product.ReconstituteParams{
		SKU:       product.SKU(r.SKU),
		Name:      r.Name,
		Price:     price,
		Active:    r.Active,
		Version:   r.Version,
		UpdatedAt: r.UpdatedAt.UTC(),
	})
}

func (i *ProductRepository) FindBySKU(c context.Context, sku product.SKU) (*product.Product, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var row productRow
	err := platformpg.TxOrDB(ctx, i.db).Take(&row, "sku = ?", sku.String()).Error
	if err != nil {
		return nil, translate(fmt.Errorf("find product %s: %w", sku, err), product.ErrNotFound, nil)
	}

	return row.toAggregate()
}

// FindBySKUs is one query (sku IN): the batch a price quote needs.
func (i *ProductRepository) FindBySKUs(c context.Context, skus []product.SKU) (map[product.SKU]*product.Product, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make(map[product.SKU]*product.Product, len(skus))
	if len(skus) == 0 {
		return out, nil
	}

	keys := make([]string, len(skus))
	for n, sku := range skus {
		keys[n] = sku.String()
	}
	var rows []productRow
	if err := platformpg.TxOrDB(ctx, i.db).Where("sku IN ?", keys).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("find %d products: %w", len(skus), err)
	}
	for _, row := range rows {
		p, err := row.toAggregate()
		if err != nil {
			return nil, err
		}
		out[p.SKU()] = p
	}

	return out, nil
}

// likeEscape turns a literal string into a LIKE pattern fragment (ESCAPE '\').
var likeEscape = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func (i *ProductRepository) List(c context.Context, p product.ListParams) ([]*product.Product, product.SKU, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	q := platformpg.TxOrDB(ctx, i.db).Order("sku ASC")
	if p.After != "" {
		q = q.Where("sku > ?", p.After.String())
	}
	if p.SKUPrefix != "" {
		// Literal prefix: escape LIKE's wildcards so "SKU_1" does not match "SKU-1".
		q = q.Where("sku LIKE ? ESCAPE '\\'", likeEscape.Replace(p.SKUPrefix)+"%")
	}
	if p.Limit > 0 {
		q = q.Limit(p.Limit + 1)
	}
	var rows []productRow
	if err := q.Find(&rows).Error; err != nil {
		return nil, "", fmt.Errorf("list products: %w", err)
	}
	hasMore := p.Limit > 0 && len(rows) > p.Limit
	if hasMore {
		rows = rows[:p.Limit]
	}
	out := make([]*product.Product, 0, len(rows))
	for _, row := range rows {
		pr, err := row.toAggregate()
		if err != nil {
			return nil, "", err
		}
		out = append(out, pr)
	}
	var next product.SKU
	if hasMore {
		next = out[len(out)-1].SKU()
	}

	return out, next, nil
}

// Save inserts a never-saved product (Version 0) with version 1, or updates a
// loaded one `WHERE version = <loaded>`, bumping it. Both ways a lost race is
// ErrStale: a second insert of the same SKU hits the primary key, an update
// whose version moved on matches zero rows. Either way the caller reloads and
// retries (UpsertProduct then updates the row the other writer created).
func (i *ProductRepository) Save(c context.Context, p *product.Product) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	db := platformpg.TxOrDB(ctx, i.db)
	if p.Version() == 0 {
		row := productRow{
			SKU: p.SKU().String(), Name: p.Name(),
			PriceAmount: p.Price().Amount(), PriceCurrency: p.Price().Currency(),
			Active: p.Active(), Version: 1, UpdatedAt: p.UpdatedAt(),
		}
		if err := db.Create(&row).Error; err != nil {
			return translate(fmt.Errorf("insert product %s: %w", p.SKU(), err), nil, fmt.Errorf("insert product %s: %w", p.SKU(), product.ErrStale))
		}

		return nil
	}

	res := db.Model(&productRow{}).
		Where("sku = ? AND version = ?", p.SKU().String(), p.Version()).
		Updates(map[string]any{
			"name":           p.Name(),
			"price_amount":   p.Price().Amount(),
			"price_currency": p.Price().Currency(),
			"active":         p.Active(),
			"updated_at":     p.UpdatedAt(),
			"version":        gorm.Expr("version + 1"),
		})
	if res.Error != nil {
		return fmt.Errorf("update product %s: %w", p.SKU(), res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("update product %s: %w", p.SKU(), product.ErrStale)
	}

	return nil
}
