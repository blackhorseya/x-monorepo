package order

import "github.com/blackhorseya/x-monorepo/pkgs/kernel"

var (
	ErrInvalidID       = kernel.NewError(kernel.KindInvalidArgument, "order.invalid_id", "order id must not be empty")
	ErrEmptyCustomer   = kernel.NewError(kernel.KindInvalidArgument, "order.empty_customer", "customer id must not be empty")
	ErrNoItems         = kernel.NewError(kernel.KindInvalidArgument, "order.no_items", "order must contain at least one item")
	ErrInvalidSKU      = kernel.NewError(kernel.KindInvalidArgument, "order.invalid_sku", "sku must not be empty")
	ErrInvalidQuantity = kernel.NewError(kernel.KindInvalidArgument, "order.invalid_quantity", "quantity must be positive")
	ErrCannotCancel    = kernel.NewError(kernel.KindFailedPrecondition, "order.cannot_cancel", "order cannot be cancelled in its current status")
	ErrNotFound        = kernel.NewError(kernel.KindNotFound, "order.not_found", "order not found")
	// ErrInvalidStatusFilter is a list filter naming a status this version does not know.
	ErrInvalidStatusFilter = kernel.NewError(kernel.KindInvalidArgument, "order.invalid_status_filter", "unknown order status filter")
	ErrInvalidCursor       = kernel.NewError(kernel.KindInvalidArgument, "order.invalid_cursor", "cursor must carry a timestamp and an id")
	// ErrDuplicate is returned by Repository.Save when another order already owns the id.
	ErrDuplicate = kernel.NewError(kernel.KindConflict, "order.duplicate", "order already exists")
	// ErrInvalidStatus means persisted state names a status this version does not know.
	ErrInvalidStatus = kernel.NewError(kernel.KindInternal, "order.invalid_status", "unknown order status")
)
