package postgres

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

var _ reservation.Repository = (*ReservationRepository)(nil)

// ReservationRepository persists Reservation across reservations + reservation_lines.
type ReservationRepository struct {
	db *gorm.DB
}

func NewReservationRepository(db *gorm.DB) *ReservationRepository {
	return &ReservationRepository{db: db}
}

type reservationRow struct {
	OrderID   string               `gorm:"primaryKey"`
	Status    string               `gorm:"not null"`
	CreatedAt time.Time            `gorm:"not null;autoCreateTime:false"`
	UpdatedAt time.Time            `gorm:"not null;autoUpdateTime:false"`
	Lines     []reservationLineRow `gorm:"foreignKey:OrderID;references:OrderID"`
}

func (reservationRow) TableName() string { return Schema + ".reservations" }

// reservationLineRow keeps the shortfall per line (short = true) instead of a
// separate array column: the aggregate rebuilds its SKU list from it.
type reservationLineRow struct {
	OrderID  string `gorm:"primaryKey"`
	Position int    `gorm:"primaryKey"`
	SKU      string `gorm:"not null"`
	Quantity int32  `gorm:"not null"`
	Short    bool   `gorm:"not null"`
}

func (reservationLineRow) TableName() string { return Schema + ".reservation_lines" }

func (i *ReservationRepository) FindByOrderID(c context.Context, orderID string) (*reservation.Reservation, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var row reservationRow
	err := platformpg.TxOrDB(ctx, i.db).
		Preload("Lines", func(db *gorm.DB) *gorm.DB { return db.Order("position") }).
		Take(&row, "order_id = ?", orderID).Error
	if err != nil {
		return nil, translate(fmt.Errorf("find reservation %s: %w", orderID, err), reservation.ErrNotFound, nil)
	}

	return toDomain(row)
}

// FindByOrderIDs is one primary-key lookup (order_id IN (...)) plus one for the
// lines, however many ids: the batch behind the console's orders list.
func (i *ReservationRepository) FindByOrderIDs(c context.Context, orderIDs []string) ([]*reservation.Reservation, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(orderIDs) == 0 {
		return nil, nil
	}

	var rows []reservationRow
	err := platformpg.TxOrDB(ctx, i.db).
		Preload("Lines", func(db *gorm.DB) *gorm.DB { return db.Order("position") }).
		Where("order_id IN ?", orderIDs).
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("find %d reservations: %w", len(orderIDs), err)
	}
	out := make([]*reservation.Reservation, 0, len(rows))
	for _, row := range rows {
		r, err := toDomain(row)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}

	return out, nil
}

// Add inserts the aggregate; the primary key turns a race into ErrDuplicate.
func (i *ReservationRepository) Add(c context.Context, r *reservation.Reservation) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	row := toRow(r)
	err := platformpg.TxOrDB(ctx, i.db).Create(&row).Error
	if err != nil {
		return translate(fmt.Errorf("add reservation %s: %w", r.OrderID(), err), nil, reservation.ErrDuplicate)
	}

	return nil
}

// Update stores a status change; lines are immutable after Add. It is a
// compare-and-swap on status: the row only changes while it still has the
// status the aggregate was loaded with, otherwise ErrStale. Every transition is
// one-way, so the status alone detects a reservation that moved (expired,
// committed, released) between this transaction's read and its write.
func (i *ReservationRepository) Update(c context.Context, r *reservation.Reservation) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	res := platformpg.TxOrDB(ctx, i.db).Model(&reservationRow{}).
		Where("order_id = ? AND status = ?", r.OrderID(), r.LoadedStatus().String()).
		Updates(map[string]any{"status": r.Status().String(), "updated_at": r.UpdatedAt()})
	if res.Error != nil {
		return fmt.Errorf("update reservation %s: %w", r.OrderID(), res.Error)
	}
	if res.RowsAffected == 0 {
		var n int64
		if err := platformpg.TxOrDB(ctx, i.db).Model(&reservationRow{}).Where("order_id = ?", r.OrderID()).Count(&n).Error; err != nil {
			return fmt.Errorf("update reservation %s: %w", r.OrderID(), err)
		}
		if n == 0 {
			return reservation.ErrNotFound
		}

		return fmt.Errorf("update reservation %s: %w", r.OrderID(), reservation.ErrStale)
	}

	return nil
}

func (i *ReservationRepository) List(c context.Context, p reservation.ListParams) ([]*reservation.Reservation, *reservation.Cursor, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	q := platformpg.TxOrDB(ctx, i.db).
		Preload("Lines", func(db *gorm.DB) *gorm.DB { return db.Order("position") }).
		Order("created_at DESC, order_id DESC")
	if p.Status != nil {
		q = q.Where("status = ?", p.Status.String())
	}
	if p.After != nil {
		q = q.Where("(created_at, order_id) < (?, ?)", p.After.CreatedAt(), p.After.OrderID())
	}
	if p.Limit > 0 {
		q = q.Limit(p.Limit + 1)
	}
	var rows []reservationRow
	if err := q.Find(&rows).Error; err != nil {
		return nil, nil, fmt.Errorf("list reservations: %w", err)
	}
	hasMore := p.Limit > 0 && len(rows) > p.Limit
	if hasMore {
		rows = rows[:p.Limit]
	}
	out := make([]*reservation.Reservation, 0, len(rows))
	for _, row := range rows {
		r, err := toDomain(row)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, r)
	}
	var next *reservation.Cursor
	if hasMore {
		cur := reservation.CursorOf(out[len(out)-1])
		next = &cur
	}

	return out, next, nil
}

// ListReservedBefore is the expiry sweep's input: RESERVED rows whose hold
// (updated_at, set by Reserve) started before the instant, oldest first. The
// partial index of 0007 serves it.
func (i *ReservationRepository) ListReservedBefore(c context.Context, before time.Time, limit int) ([]*reservation.Reservation, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	q := platformpg.TxOrDB(ctx, i.db).
		Preload("Lines", func(db *gorm.DB) *gorm.DB { return db.Order("position") }).
		Where("status = ? AND updated_at < ?", reservation.StatusReserved.String(), before).
		Order("updated_at ASC, order_id ASC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	var rows []reservationRow
	if err := q.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list reserved before %s: %w", before.UTC().Format(time.RFC3339), err)
	}
	out := make([]*reservation.Reservation, 0, len(rows))
	for _, row := range rows {
		r, err := toDomain(row)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}

	return out, nil
}

func toRow(r *reservation.Reservation) reservationRow {
	short := make(map[string]bool, len(r.Shortfall()))
	for _, sku := range r.Shortfall() {
		short[sku] = true
	}
	lines := r.Lines()
	row := reservationRow{
		OrderID:   r.OrderID(),
		Status:    r.Status().String(),
		CreatedAt: r.CreatedAt(),
		UpdatedAt: r.UpdatedAt(),
		Lines:     make([]reservationLineRow, 0, len(lines)),
	}
	for pos, l := range lines {
		row.Lines = append(row.Lines, reservationLineRow{
			OrderID:  row.OrderID,
			Position: pos,
			SKU:      l.SKU(),
			Quantity: l.Quantity(),
			Short:    short[l.SKU()],
		})
	}

	return row
}

func toDomain(row reservationRow) (*reservation.Reservation, error) {
	lines := make([]reservation.Line, 0, len(row.Lines))
	var shortfall []string
	seen := map[string]bool{}
	for _, l := range row.Lines {
		line, err := reservation.NewLine(l.SKU, l.Quantity)
		if err != nil {
			return nil, fmt.Errorf("load reservation %s line %s: %w", row.OrderID, l.SKU, err)
		}
		lines = append(lines, line)
		if l.Short && !seen[l.SKU] {
			seen[l.SKU] = true
			shortfall = append(shortfall, l.SKU)
		}
	}

	status, err := reservation.ParseStatus(row.Status)
	if err != nil {
		return nil, fmt.Errorf("load reservation %s: %w", row.OrderID, err)
	}

	return reservation.Reconstitute(reservation.ReconstituteParams{
		OrderID:   row.OrderID,
		Lines:     lines,
		Status:    status,
		Shortfall: shortfall,
		CreatedAt: row.CreatedAt.UTC(),
		UpdatedAt: row.UpdatedAt.UTC(),
	})
}

// CountByStatus is one GROUP BY over the reservations table; statuses with no row are absent.
func (i *ReservationRepository) CountByStatus(c context.Context) ([]reservation.StatusCount, error) {
	ctx := contextx.FromContext(c)

	var rows []struct {
		Status string
		N      int64
		Oldest time.Time
	}
	err := platformpg.TxOrDB(ctx, i.db).
		Raw(`SELECT status, count(*) AS n, min(updated_at) AS oldest FROM ` + Schema + `.reservations GROUP BY status ORDER BY status`).
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("count reservations by status: %w", err)
	}

	out := make([]reservation.StatusCount, 0, len(rows))
	for _, r := range rows {
		st, err := reservation.ParseStatus(r.Status)
		if err != nil {
			return nil, err
		}
		out = append(out, reservation.NewStatusCount(st, r.N, r.Oldest))
	}

	return out, nil
}
