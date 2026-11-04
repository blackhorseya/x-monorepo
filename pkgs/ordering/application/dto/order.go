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

// OrderLine is one line of a CreateOrder request: what and how many. The price is not the
// caller's to give; the use case asks the catalog.
type OrderLine struct {
	SKU      string
	Quantity int32
}

type CreateOrderInput struct {
	CustomerID string
	Items      []OrderLine
	// IdempotencyKey, when set, makes CreateOrder safe to retry: see usecase.CreateOrder.
	IdempotencyKey string
}

// PriceQuote is the catalog's answer for a set of SKUs: the price of each active product,
// and the SKUs it cannot quote, split into those it has no product for (Missing) and those
// taken off sale (Inactive), in the order asked.
type PriceQuote struct {
	Prices   map[string]Money
	Missing  []string
	Inactive []string
}

type GetOrderInput struct {
	OrderID string
}

// CancelSource says who is asking for a cancellation. CancelOrder is shared by the rpc adapter and
// the consumer adapter and cannot tell them apart; each says so here. The zero value is refused
// (order.ErrInvalidCause), never defaulted.
type CancelSource uint8

const (
	CancelSourceUnspecified CancelSource = iota
	// CancelSourceRPC: a client's CancelOrder request.
	CancelSourceRPC
	// CancelSourceCancellationRequest: another BC's OrderCancellationRequested.
	CancelSourceCancellationRequest
)

type CancelOrderInput struct {
	OrderID string
	// Source is set by the adapter that drives the use case; it becomes the history's cause.
	Source CancelSource
	// Reason is upstream free text: informational only, never logged or stored (#427).
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

// PaymentCapturedInput is driven by the consumer adapter only (payment.v1.PaymentCaptured).
type PaymentCapturedInput struct {
	OrderID string
}

// PaymentExpiredInput is driven by the consumer adapter only (payment.v1.PaymentExpired).
type PaymentExpiredInput struct {
	OrderID string
}

// PaymentEventOutput says what the event did: "applied", "ignored", "noop" or "unexpected"
// (order.PaymentEffect).
type PaymentEventOutput struct {
	Effect string
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
	Status     string // "" = any; PENDING | CONFIRMED | PAID | COMPLETED | CANCELLED
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
	Status     string // PENDING | CONFIRMED | PAID | COMPLETED | CANCELLED
	CreatedAt  time.Time
	UpdatedAt  time.Time
	// PaymentRequired is the snapshot of ordering.require_payment taken at CONFIRMED.
	PaymentRequired bool
	// Replayed is true when CreateOrder answered with an order an earlier call
	// with the same idempotency key had already created (nothing was written).
	Replayed bool
}
