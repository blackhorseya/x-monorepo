// Package dto is the application boundary of the inventory bounded context.
// Rule: carry only what a use case needs, never mirror the proto contract.
package dto

import "time"

type Line struct {
	SKU      string
	Quantity int32
}

// ReserveStockInput is what ordering.v1.OrderCreated boils down to for inventory.
type ReserveStockInput struct {
	OrderID string
	Lines   []Line
}

type ReleaseStockInput struct {
	OrderID string
}

// CommitStockInput is what ordering.v1.OrderCompleted boils down to for inventory.
type CommitStockInput struct {
	OrderID string
}

// ExpireReservationsInput is one sweep: RESERVED reservations held at least
// OlderThan, oldest first, at most Limit (0 = default 100, capped at 1000).
// DryRun lists them and changes nothing.
type ExpireReservationsInput struct {
	OlderThan time.Duration
	Limit     int
	DryRun    bool
}

// ExpiredReservation is one reservation a sweep expired (or would).
type ExpiredReservation struct {
	OrderID    string
	ReservedAt time.Time
	HeldFor    time.Duration
}

type ExpireReservationsOutput struct {
	Expired []ExpiredReservation // oldest first; len == Limit means there may be more
	DryRun  bool
}

// ReservationStatsInput has no fields: the stats are for the whole BC.
type ReservationStatsInput struct{}

// StatusStat is one status of the funnel: how many reservations, and when the
// oldest of them last changed (zero when Count is 0). Every known status is present.
type StatusStat struct {
	Status          string
	Count           int64
	OldestUpdatedAt time.Time
}

type ReservationStatsOutput struct {
	Statuses []StatusStat
}

type GetReservationInput struct {
	OrderID string
}

type ReservationOutput struct {
	OrderID       string
	Lines         []Line
	Status        string   // RESERVED | REJECTED | RELEASED | COMMITTED | EXPIRED | CANCELLED
	ShortfallSKUs []string // REJECTED only
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type SetStockInput struct {
	SKU    string
	OnHand int32
}

type GetStockInput struct {
	SKU string
}

type ListReservationsInput struct {
	Status    string // "" = any; RESERVED | REJECTED | RELEASED | COMMITTED | EXPIRED | CANCELLED
	PageSize  int32  // 0 = default
	PageToken string // "" = first page
}

type ListReservationsOutput struct {
	Reservations  []ReservationOutput
	NextPageToken string // "" = last page
}

type ListStocksInput struct {
	PageSize     int32  // 0 = default
	PageToken    string // "" = first page; carries the filters it was minted with
	SKUPrefix    string // "" = every SKU
	OnlyReserved bool   // only SKUs with reserved > 0
}

type ListStocksOutput struct {
	Stocks        []StockOutput
	NextPageToken string // "" = last page
}

type StockOutput struct {
	SKU       string
	OnHand    int32
	Reserved  int32
	Available int32
	UpdatedAt time.Time
}
