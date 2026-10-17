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

// Update stores a status change; lines are immutable after Add.
func (i *ReservationRepository) Update(c context.Context, r *reservation.Reservation) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	res := platformpg.TxOrDB(ctx, i.db).Model(&reservationRow{}).
		Where("order_id = ?", r.OrderID()).
		Updates(map[string]any{"status": r.Status().String(), "updated_at": r.UpdatedAt()})
	if res.Error != nil {
		return fmt.Errorf("update reservation %s: %w", r.OrderID(), res.Error)
	}
	if res.RowsAffected == 0 {
		return reservation.ErrNotFound
	}

	return nil
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
