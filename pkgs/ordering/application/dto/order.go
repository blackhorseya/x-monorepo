// Package dto is the application boundary (go-kit's endpoint request/response).
// Rule: carry only what a use case needs, never mirror the proto contract.
package dto

import "time"

type Money struct {
	AmountMinor int64
	Currency    string
}

type LineItem struct {
	SKU       string
	Quantity  int32
	UnitPrice Money
}

type CreateOrderInput struct {
	CustomerID string
	Items      []LineItem
}

type GetOrderInput struct {
	OrderID string
}

type CancelOrderInput struct {
	OrderID string
	// Reason is informational (logged), not a business rule input.
	Reason string
}

// ConfirmOrderInput is driven by the consumer adapter only (inventory.v1.StockReserved).
type ConfirmOrderInput struct {
	OrderID string
}

// CompleteOrderInput is driven by the rpc adapter: fulfil a CONFIRMED order.
type CompleteOrderInput struct {
	OrderID string
}

// OrderStatsInput has no fields: the stats are for the whole BC.
type OrderStatsInput struct{}

// StatusStat is one status of the funnel: how many orders, and when the oldest
// of them last changed (zero when Count is 0). Every known status is present.
type StatusStat struct {
	Status          string
	Count           int64
	OldestUpdatedAt time.Time
}

type OrderStatsOutput struct {
	Statuses []StatusStat
}

type ListOrdersInput struct {
	CustomerID string // "" = any
	Status     string // "" = any; PENDING | CONFIRMED | COMPLETED | CANCELLED
	PageSize   int32  // 0 = default
	PageToken  string // "" = first page
}

type ListOrdersOutput struct {
	Orders        []OrderOutput
	NextPageToken string // "" = last page
}

type OrderOutput struct {
	ID         string
	CustomerID string
	Items      []LineItem
	Total      Money
	Status     string // PENDING | CONFIRMED | COMPLETED | CANCELLED
	CreatedAt  time.Time
	UpdatedAt  time.Time
}
