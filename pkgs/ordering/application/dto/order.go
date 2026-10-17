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

type ListOrdersInput struct {
	CustomerID string // "" = any
	Status     string // "" = any; PENDING | CANCELLED
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
	Status     string // PENDING | CANCELLED
	CreatedAt  time.Time
	UpdatedAt  time.Time
}
