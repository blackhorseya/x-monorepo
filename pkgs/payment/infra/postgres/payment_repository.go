package postgres

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

var _ payment.Repository = (*PaymentRepository)(nil)

// PaymentRepository persists Payment in payment.payments with optimistic locking.
type PaymentRepository struct {
	db *gorm.DB
}

func NewPaymentRepository(db *gorm.DB) *PaymentRepository { return &PaymentRepository{db: db} }

// paymentRow mirrors columns, not the aggregate. Timestamps come from the domain (port.Clock), so
// gorm's automatic tracking is switched off.
type paymentRow struct {
	ID                string    `gorm:"primaryKey"`
	OrderID           string    `gorm:"not null"`
	CustomerID        string    `gorm:"not null"`
	Amount            int64     `gorm:"not null"`
	Currency          string    `gorm:"not null"`
	Status            string    `gorm:"not null"`
	OrderOutcome      string    `gorm:"not null"`
	ExpiresAt         time.Time `gorm:"not null"`
	Provider          string    `gorm:"not null"`
	ProviderRef       string    `gorm:"not null"`
	CheckoutURL       string    `gorm:"not null"`
	CheckoutExpiresAt *time.Time
	TerminalAt        *time.Time
	Version           int64     `gorm:"not null"`
	UpdatedAt         time.Time `gorm:"not null;autoUpdateTime:false"`
}

func (paymentRow) TableName() string { return Schema + ".payments" }

func (r paymentRow) toAggregate() (*payment.Payment, error) {
	amount, err := kernel.NewMoney(r.Amount, r.Currency)
	if err != nil {
		return nil, fmt.Errorf("payment %s: %w", r.ID, err)
	}

	return payment.Reconstitute(payment.ReconstituteParams{
		ID: payment.ID(r.ID), OrderID: r.OrderID, CustomerID: r.CustomerID, Amount: amount,
		Status: payment.Status(r.Status), ExpiresAt: r.ExpiresAt.UTC(),
		Provider: r.Provider, ProviderRef: r.ProviderRef, CheckoutURL: r.CheckoutURL,
		CheckoutExpiresAt: utc(r.CheckoutExpiresAt), Outcome: payment.Outcome(r.OrderOutcome),
		TerminalAt: utc(r.TerminalAt), Version: r.Version, UpdatedAt: r.UpdatedAt.UTC(),
	})
}

func utc(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}

	return t.UTC()
}

func orNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}

	return &t
}

func (i *PaymentRepository) FindByOrderID(c context.Context, orderID string) (*payment.Payment, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var row paymentRow
	err := platformpg.TxOrDB(ctx, i.db).Take(&row, "order_id = ?", orderID).Error
	if err != nil {
		return nil, translate(fmt.Errorf("find payment of order %s: %w", orderID, err), payment.ErrNotFound, nil)
	}

	return row.toAggregate()
}

// Save inserts a never-saved payment (Version 0) with version 1, or updates a loaded one
// `WHERE version = <loaded>`, bumping it. A second payment for the same order (or a reused id) hits
// a unique constraint and is ErrDuplicateForOrder; an update whose version moved on matches zero
// rows and is ErrStale. Either way the caller reloads or drops it.
func (i *PaymentRepository) Save(c context.Context, p *payment.Payment) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	db := platformpg.TxOrDB(ctx, i.db)
	if p.Version() == 0 {
		row := paymentRow{
			ID: p.ID().String(), OrderID: p.OrderID(), CustomerID: p.CustomerID(),
			Amount: p.Amount().Amount(), Currency: p.Amount().Currency(), Status: p.Status().String(),
			OrderOutcome: p.Outcome().String(), ExpiresAt: p.ExpiresAt(), Provider: p.Provider(),
			ProviderRef: p.ProviderRef(), CheckoutURL: p.CheckoutURL(),
			CheckoutExpiresAt: orNil(p.CheckoutExpiresAt()), TerminalAt: orNil(p.TerminalAt()),
			Version: 1, UpdatedAt: p.UpdatedAt(),
		}
		if err := db.Create(&row).Error; err != nil {
			return translate(fmt.Errorf("insert payment of order %s: %w", p.OrderID(), err), nil,
				fmt.Errorf("insert payment of order %s: %w", p.OrderID(), payment.ErrDuplicateForOrder))
		}

		return nil
	}

	res := db.Model(&paymentRow{}).
		Where("id = ? AND version = ?", p.ID().String(), p.Version()).
		Updates(map[string]any{
			"status":              p.Status().String(),
			"order_outcome":       p.Outcome().String(),
			"provider":            p.Provider(),
			"provider_ref":        p.ProviderRef(),
			"checkout_url":        p.CheckoutURL(),
			"checkout_expires_at": orNil(p.CheckoutExpiresAt()),
			"terminal_at":         orNil(p.TerminalAt()),
			"updated_at":          p.UpdatedAt(),
			"version":             gorm.Expr("version + 1"),
		})
	if res.Error != nil {
		return fmt.Errorf("update payment of order %s: %w", p.OrderID(), res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("update payment of order %s: %w", p.OrderID(), payment.ErrStale)
	}

	return nil
}
