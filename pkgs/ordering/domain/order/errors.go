package order

import (
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

var (
	ErrInvalidID       = kernel.NewError(kernel.KindInvalidArgument, "order.invalid_id", "order id must not be empty")
	ErrEmptyCustomer   = kernel.NewError(kernel.KindInvalidArgument, "order.empty_customer", "customer id must not be empty")
	ErrNoItems         = kernel.NewError(kernel.KindInvalidArgument, "order.no_items", "order must contain at least one item")
	ErrInvalidSKU      = kernel.NewError(kernel.KindInvalidArgument, "order.invalid_sku", "sku must not be empty")
	ErrInvalidQuantity = kernel.NewError(kernel.KindInvalidArgument, "order.invalid_quantity", "quantity must be positive")
	ErrCannotCancel    = kernel.NewError(kernel.KindFailedPrecondition, "order.cannot_cancel", "order cannot be cancelled in its current status")
	ErrCannotConfirm   = kernel.NewError(kernel.KindFailedPrecondition, "order.cannot_confirm", "order cannot be confirmed in its current status")
	ErrCannotComplete  = kernel.NewError(kernel.KindFailedPrecondition, "order.cannot_complete", "only a confirmed order can be completed")
	ErrNotFound        = kernel.NewError(kernel.KindNotFound, "order.not_found", "order not found")
	// ErrInvalidStatusFilter is a list filter naming a status this version does not know.
	ErrInvalidStatusFilter = kernel.NewError(kernel.KindInvalidArgument, "order.invalid_status_filter", "unknown order status filter")
	ErrInvalidCursor       = kernel.NewError(kernel.KindInvalidArgument, "order.invalid_cursor", "cursor must carry a timestamp and an id")
	// ErrStale is returned by Repository.Save when the order changed since it was
	// loaded (optimistic concurrency, e.g. two concurrent cancels). It wraps
	// kernel.ErrConcurrentUpdate and is deliberately NOT a kernel.Error: the
	// consumer must retry (reload → already cancelled → ErrCannotCancel → drop),
	// and the rpc edge answers Aborted.
	ErrStale = fmt.Errorf("order: reload and retry: %w", kernel.ErrConcurrentUpdate)

	// ErrDuplicate is returned by Repository.Save when another order already owns the id.
	ErrDuplicate = kernel.NewError(kernel.KindConflict, "order.duplicate", "order already exists")
	// ErrIdempotencyKeyReused is CreateOrder's answer to a key this customer already
	// spent on a request with different items. The message is fixed on purpose: it
	// never carries the other order, its items, or the key.
	ErrIdempotencyKeyReused = kernel.NewError(kernel.KindConflict, "order.idempotency_key_reused", "idempotency key was already used for a different request")
	// ErrIdempotencyConflict is returned by Repository.Save when a new order's
	// (customer, idempotency key) is already taken. Nothing was written; the
	// caller reads the winner back with FindByIdempotencyKey.
	ErrIdempotencyConflict = kernel.NewError(kernel.KindInternal, "order.idempotency_conflict", "idempotency key is taken")
	// ErrInvalidStatus means persisted state names a status this version does not know.
	ErrInvalidStatus = kernel.NewError(kernel.KindInternal, "order.invalid_status", "unknown order status")
)
