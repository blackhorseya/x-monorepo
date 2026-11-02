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

func (i *PaymentRepository) FindByID(c context.Context, id payment.ID) (*payment.Payment, error) {
	return i.findOne(c, "id = ?", id.String(), "payment "+id.String())
}

func (i *PaymentRepository) FindByProviderRef(c context.Context, provider, ref string) (*payment.Payment, error) {
	if ref == "" {
		return nil, payment.ErrNotFound // a payment that never opened a checkout has no reference to match
	}
	var row paymentRow
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	err := platformpg.TxOrDB(ctx, i.db).Take(&row, "provider = ? AND provider_ref = ?", provider, ref).Error
	if err != nil {
		return nil, translate(fmt.Errorf("find payment by provider reference: %w", err), payment.ErrNotFound, nil)
	}

	return row.toAggregate()
}

func (i *PaymentRepository) findOne(c context.Context, where string, arg any, what string) (*payment.Payment, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var row paymentRow
	if err := platformpg.TxOrDB(ctx, i.db).Take(&row, where, arg).Error; err != nil {
		return nil, translate(fmt.Errorf("find %s: %w", what, err), payment.ErrNotFound, nil)
	}

	return row.toAggregate()
}

func (i *PaymentRepository) ListPendingDue(c context.Context, now time.Time, limit int) ([]*payment.Payment, error) {
	return i.list(c, limit, "status = 'PENDING' AND expires_at <= ?", now, "expires_at ASC")
}

func (i *PaymentRepository) ListReconcilable(c context.Context, notBefore time.Time, limit int) ([]*payment.Payment, error) {
	return i.list(c, limit, "status IN ('EXPIRED', 'CANCELLED') AND provider_ref <> '' AND terminal_at >= ?", notBefore, "terminal_at ASC")
}

func (i *PaymentRepository) list(c context.Context, limit int, where string, arg any, order string) ([]*payment.Payment, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var rows []paymentRow
	q := platformpg.TxOrDB(ctx, i.db).Where(where, arg).Order(order + ", id ASC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list payments: %w", err)
	}
	out := make([]*payment.Payment, 0, len(rows))
	for _, r := range rows {
		p, err := r.toAggregate()
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}

	return out, nil
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

var _ payment.OutcomeRepository = (*OutcomeRepository)(nil)

// OutcomeRepository keeps payment.order_outcomes.
type OutcomeRepository struct {
	db *gorm.DB
}

func NewOutcomeRepository(db *gorm.DB) *OutcomeRepository { return &OutcomeRepository{db: db} }

func (i *OutcomeRepository) Find(c context.Context, orderID string) (payment.Outcome, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return payment.OutcomeNone, err
	}
	var outcome string
	res := platformpg.TxOrDB(ctx, i.db).Raw(`SELECT outcome FROM `+Schema+`.order_outcomes WHERE order_id = ?`, orderID).Scan(&outcome)
	if res.Error != nil {
		return payment.OutcomeNone, fmt.Errorf("find outcome of order %s: %w", orderID, res.Error)
	}
	if res.RowsAffected == 0 {
		return payment.OutcomeNone, nil
	}

	return payment.Outcome(outcome), nil
}

// Lock takes a transaction-scoped advisory lock keyed by the order id: it is released at commit or
// rollback, whichever way the surrounding transaction ends, and nothing outlives a crashed connection.
// The two-key form (class, object) lives in its own key space, apart from the one-bigint form, and the
// class is payment's own, so no other lock of this database can be the same lock.
func (i *OutcomeRepository) Lock(c context.Context, orderID string) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := platformpg.TxOrDB(ctx, i.db).Exec(`SELECT pg_advisory_xact_lock(hashtext(?), hashtext(?))`, Schema+".order", orderID).Error; err != nil {
		return fmt.Errorf("lock order %s: %w", orderID, err)
	}

	return nil
}

func (i *OutcomeRepository) Record(c context.Context, orderID string, outcome payment.Outcome, at time.Time) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}
	err := platformpg.TxOrDB(ctx, i.db).Exec(
		`INSERT INTO `+Schema+`.order_outcomes (order_id, outcome, recorded_at) VALUES (?, ?, ?) ON CONFLICT (order_id) DO NOTHING`,
		orderID, outcome.String(), at).Error
	if err != nil {
		return fmt.Errorf("record outcome of order %s: %w", orderID, err)
	}

	return nil
}

var _ payment.WebhookRepository = (*WebhookRepository)(nil)

// WebhookRepository keeps payment.webhook_events.
type WebhookRepository struct {
	db *gorm.DB
}

func NewWebhookRepository(db *gorm.DB) *WebhookRepository { return &WebhookRepository{db: db} }

type webhookRow struct {
	Provider        string `gorm:"primaryKey"`
	ProviderEventID string `gorm:"primaryKey"`
	EventType       string
	ProviderRef     string
	Amount          *int64
	Currency        *string
	Body            []byte
	ReceivedAt      time.Time
	ProcessedAt     *time.Time
}

func (webhookRow) TableName() string { return Schema + ".webhook_events" }

func (i *WebhookRepository) Insert(c context.Context, w *payment.WebhookEvent) (bool, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var amount *int64
	var currency *string
	if w.Amount() != (kernel.Money{}) {
		a, cur := w.Amount().Amount(), w.Amount().Currency()
		amount, currency = &a, &cur
	}
	res := platformpg.TxOrDB(ctx, i.db).Exec(
		`INSERT INTO `+Schema+`.webhook_events (provider, provider_event_id, event_type, provider_ref, amount, currency, body, received_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (provider, provider_event_id) DO NOTHING`,
		w.Provider(), w.EventID(), w.EventType(), w.Ref(), amount, currency, w.Body(), w.ReceivedAt())
	if res.Error != nil {
		return false, fmt.Errorf("insert webhook event: %w", res.Error)
	}

	return res.RowsAffected == 1, nil
}

func (i *WebhookRepository) Find(c context.Context, provider, eventID string) (*payment.WebhookEvent, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var row webhookRow
	if err := platformpg.TxOrDB(ctx, i.db).Take(&row, "provider = ? AND provider_event_id = ?", provider, eventID).Error; err != nil {
		return nil, translate(fmt.Errorf("find webhook event: %w", err), payment.ErrNotFound, nil)
	}
	var amount kernel.Money
	if row.Amount != nil && row.Currency != nil {
		m, err := kernel.NewMoney(*row.Amount, *row.Currency)
		if err != nil {
			return nil, fmt.Errorf("webhook event: %w", err)
		}
		amount = m
	}

	return payment.ReconstituteWebhook(payment.ReconstituteWebhookParams{
		Provider: row.Provider, EventID: row.ProviderEventID, EventType: row.EventType, Ref: row.ProviderRef,
		Amount: amount, Body: row.Body, ReceivedAt: row.ReceivedAt.UTC(), ProcessedAt: utc(row.ProcessedAt),
	})
}

func (i *WebhookRepository) MarkProcessed(c context.Context, provider, eventID string, at time.Time) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}
	err := platformpg.TxOrDB(ctx, i.db).Exec(
		`UPDATE `+Schema+`.webhook_events SET processed_at = ? WHERE provider = ? AND provider_event_id = ? AND processed_at IS NULL`,
		at, provider, eventID).Error
	if err != nil {
		return fmt.Errorf("mark webhook event processed: %w", err)
	}

	return nil
}

func (i *WebhookRepository) DeleteProcessedBefore(c context.Context, cutoff time.Time, limit int) (int, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	res := platformpg.TxOrDB(ctx, i.db).Exec(
		`DELETE FROM `+Schema+`.webhook_events WHERE (provider, provider_event_id) IN (
		   SELECT provider, provider_event_id FROM `+Schema+`.webhook_events
		   WHERE processed_at IS NOT NULL AND processed_at < ?
		     AND NOT EXISTS (SELECT 1 FROM `+Schema+`.payments p
		                     WHERE p.provider = webhook_events.provider AND p.provider_ref = webhook_events.provider_ref
		                       AND p.status = 'NEEDS_REVIEW')
		   ORDER BY processed_at LIMIT ?)`, cutoff, limit)
	if res.Error != nil {
		return 0, fmt.Errorf("delete processed webhook events: %w", res.Error)
	}

	return int(res.RowsAffected), nil
}
