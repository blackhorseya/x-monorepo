package postgres

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

var _ port.HistoryStore = (*HistoryStore)(nil)

// HistoryStore persists ordering.order_history (migration 0010). It joins the transaction in the
// context, so a history row commits or rolls back with the order and its outbox rows.
type HistoryStore struct {
	db *gorm.DB
}

func NewHistoryStore(db *gorm.DB) *HistoryStore { return &HistoryStore{db: db} }

type historyRow struct {
	OrderID      string    `gorm:"column:order_id"`
	Seq          int       `gorm:"column:seq"`
	FromStatus   *string   `gorm:"column:from_status"`
	ToStatus     string    `gorm:"column:to_status"`
	OccurredAt   time.Time `gorm:"column:occurred_at"`
	ActorKind    string    `gorm:"column:actor_kind"`
	ActorSubject string    `gorm:"column:actor_subject"`
	Cause        string    `gorm:"column:cause"`
}

func (historyRow) TableName() string { return Schema + ".order_history" }

// Append writes each entry as the order's next seq: one INSERT … SELECT MAX(seq)+1 per entry, in
// order, so the entries of one call get consecutive numbers. It must follow the order's Save in the
// same transaction (see port.HistoryStore); a primary-key collision is returned wrapped, never
// translated into a domain error and never retried.
func (i *HistoryStore) Append(c context.Context, entries ...order.HistoryEntry) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	for _, e := range entries {
		var from *string
		if e.From() != order.StatusUnknown {
			s := e.From().String()
			from = &s
		}
		err := platformpg.TxOrDB(ctx, i.db).Exec(
			`INSERT INTO `+Schema+`.order_history (order_id, seq, event, from_status, to_status, occurred_at, actor_kind, actor_subject, cause)
			 SELECT ?, COALESCE(MAX(seq), 0) + 1, ?, ?, ?, ?, ?, ?, ? FROM `+Schema+`.order_history WHERE order_id = ?`,
			string(e.OrderID()), e.Event(), from, e.To().String(), e.OccurredAt(), e.ActorKind().String(), e.ActorSubject(), e.Cause().String(), string(e.OrderID()),
		).Error
		if err != nil {
			return fmt.Errorf("append history of order %s: %w", e.OrderID(), err)
		}
	}

	return nil
}

func (i *HistoryStore) List(c context.Context, id order.ID) ([]order.HistoryEntry, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var rows []historyRow
	if err := platformpg.TxOrDB(ctx, i.db).Where("order_id = ?", string(id)).Order("seq").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list history of order %s: %w", id, err)
	}

	out := make([]order.HistoryEntry, 0, len(rows))
	for _, r := range rows {
		e, err := toHistoryEntry(r)
		if err != nil {
			return nil, fmt.Errorf("history of order %s seq %d: %w", id, r.Seq, err)
		}
		out = append(out, e)
	}

	return out, nil
}

func toHistoryEntry(r historyRow) (order.HistoryEntry, error) {
	from := order.StatusUnknown
	if r.FromStatus != nil {
		s, err := order.ParseStatus(*r.FromStatus)
		if err != nil {
			return order.HistoryEntry{}, err
		}
		from = s
	}
	to, err := order.ParseStatus(r.ToStatus)
	if err != nil {
		return order.HistoryEntry{}, err
	}
	kind, err := order.ParseActorKind(r.ActorKind)
	if err != nil {
		return order.HistoryEntry{}, err
	}
	cause, err := order.ParseCause(r.Cause)
	if err != nil {
		return order.HistoryEntry{}, err
	}

	return order.ReconstituteHistoryEntry(order.HistoryParams{
		OrderID: order.ID(r.OrderID), Seq: r.Seq, From: from, To: to, OccurredAt: r.OccurredAt.UTC(),
		ActorKind: kind, ActorSubject: r.ActorSubject, Cause: cause,
	})
}
