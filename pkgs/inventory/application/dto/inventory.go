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

type GetReservationInput struct {
	OrderID string
}

type ReservationOutput struct {
	OrderID       string
	Lines         []Line
	Status        string   // RESERVED | REJECTED | RELEASED
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

type StockOutput struct {
	SKU       string
	OnHand    int32
	Reserved  int32
	Available int32
	UpdatedAt time.Time
}
