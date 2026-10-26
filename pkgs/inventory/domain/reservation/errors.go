package reservation

import "github.com/blackhorseya/x-monorepo/pkgs/kernel"

var (
	ErrInvalidOrderID  = kernel.NewError(kernel.KindInvalidArgument, "reservation.invalid_order_id", "order id must not be empty")
	ErrNoLines         = kernel.NewError(kernel.KindInvalidArgument, "reservation.no_lines", "reservation must contain at least one line")
	ErrInvalidSKU      = kernel.NewError(kernel.KindInvalidArgument, "reservation.invalid_sku", "sku must not be empty")
	ErrInvalidQuantity = kernel.NewError(kernel.KindInvalidArgument, "reservation.invalid_quantity", "quantity must be positive")
	ErrNotReserved     = kernel.NewError(kernel.KindFailedPrecondition, "reservation.not_reserved", "only a RESERVED reservation can be released, committed or expired")
	// ErrInvalidOlderThan is an expiry sweep asked for a non-positive hold time.
	ErrInvalidOlderThan = kernel.NewError(kernel.KindInvalidArgument, "reservation.invalid_older_than", "older-than must be positive")
	ErrStockMissing     = kernel.NewError(kernel.KindFailedPrecondition, "reservation.stock_missing", "stock of a reserved sku is missing")
	ErrNotFound         = kernel.NewError(kernel.KindNotFound, "reservation.not_found", "reservation not found")
	// ErrDuplicate is returned by Repository.Save when the order already has a reservation.
	ErrDuplicate = kernel.NewError(kernel.KindConflict, "reservation.duplicate", "reservation already exists for this order")
	// ErrInvalidStatusFilter is a list filter naming a status this version does not know.
	ErrInvalidStatusFilter = kernel.NewError(kernel.KindInvalidArgument, "reservation.invalid_status_filter", "unknown reservation status filter")
	// ErrInvalidCursor is a list cursor missing its timestamp or order id.
	ErrInvalidCursor = kernel.NewError(kernel.KindInvalidArgument, "reservation.invalid_cursor", "cursor must carry a timestamp and an order id")
	// ErrInvalidStatus means persisted state names a status this version does not know.
	ErrInvalidStatus = kernel.NewError(kernel.KindInternal, "reservation.invalid_status", "unknown reservation status")
)
