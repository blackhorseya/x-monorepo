package product

import (
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

var (
	ErrInvalidSKU   = kernel.NewError(kernel.KindInvalidArgument, "product.invalid_sku", "sku must be 1 to 64 bytes")
	ErrInvalidName  = kernel.NewError(kernel.KindInvalidArgument, "product.invalid_name", "name must be 1 to 200 characters and not blank")
	ErrInvalidPrice = kernel.NewError(kernel.KindInvalidArgument, "product.invalid_price", "price must be built with kernel.NewMoney")
	ErrNotFound     = kernel.NewError(kernel.KindNotFound, "product.not_found", "product not found")

	// ErrStale is returned by Repository.Save when the row changed since it was
	// loaded (optimistic concurrency). It wraps kernel.ErrConcurrentUpdate and is
	// deliberately NOT a kernel.Error: a lost race must be retried, not reported
	// as a final domain failure.
	ErrStale = fmt.Errorf("product: reload and retry: %w", kernel.ErrConcurrentUpdate)
)
