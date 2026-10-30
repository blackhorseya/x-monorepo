package stock

import (
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

var (
	ErrInvalidSKU             = kernel.NewError(kernel.KindInvalidArgument, "stock.invalid_sku", "sku must be 1 to 64 bytes")
	ErrNegativeQuantity       = kernel.NewError(kernel.KindInvalidArgument, "stock.negative_quantity", "quantity must not be negative")
	ErrInvalidQuantity        = kernel.NewError(kernel.KindInvalidArgument, "stock.invalid_quantity", "quantity must be positive")
	ErrBelowReserved          = kernel.NewError(kernel.KindFailedPrecondition, "stock.below_reserved", "on-hand quantity cannot drop below the reserved quantity")
	ErrReleaseExceedsReserved = kernel.NewError(kernel.KindFailedPrecondition, "stock.release_exceeds_reserved", "cannot release more than is reserved")
	ErrCommitExceedsReserved  = kernel.NewError(kernel.KindFailedPrecondition, "stock.commit_exceeds_reserved", "cannot commit more than is reserved")
	ErrNotFound               = kernel.NewError(kernel.KindNotFound, "stock.not_found", "stock not found")
	// ErrCorruptQuantities means persisted state breaks the invariant 0 <= reserved <= on_hand.
	ErrCorruptQuantities = kernel.NewError(kernel.KindInternal, "stock.corrupt_quantities", "persisted stock quantities are inconsistent")

	// ErrStale is returned by Repository.Save when the row changed since it was
	// loaded (optimistic concurrency). It wraps kernel.ErrConcurrentUpdate and is
	// deliberately NOT a kernel.Error: consumers treat kernel errors as final and
	// drop the message, while a lost race must be retried.
	ErrStale = fmt.Errorf("stock: reload and retry: %w", kernel.ErrConcurrentUpdate)
)
