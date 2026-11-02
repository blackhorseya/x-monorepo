package payment

import (
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

// Codes are the authority of docs/payment.md §3 (錯誤碼表); the message of a payment error never
// carries data of the payment or of the provider.
var (
	ErrInvalidID         = kernel.NewError(kernel.KindInvalidArgument, "payment.invalid_id", "payment id must not be empty")
	ErrInvalidOrderID    = kernel.NewError(kernel.KindInvalidArgument, "payment.invalid_order_id", "order id must not be empty")
	ErrEmptyCustomer     = kernel.NewError(kernel.KindInvalidArgument, "payment.empty_customer", "customer id must not be empty")
	ErrInvalidAmount     = kernel.NewError(kernel.KindInvalidArgument, "payment.invalid_amount", "amount must be a valid non-negative amount with a 3-letter currency")
	ErrInvalidExpiry     = kernel.NewError(kernel.KindInvalidArgument, "payment.invalid_expiry", "the payment window must end after it starts")
	ErrInvalidStatus     = kernel.NewError(kernel.KindInvalidArgument, "payment.invalid_status", "unknown payment status")
	ErrInvalidOutcome    = kernel.NewError(kernel.KindInvalidArgument, "payment.invalid_outcome", "the order outcome does not fit the payment status")
	ErrInvalidCheckout   = kernel.NewError(kernel.KindInvalidArgument, "payment.invalid_checkout", "the checkout needs a provider, a reference, a url and a later expiry")
	ErrMissingTotal      = kernel.NewError(kernel.KindInvalidArgument, "payment.missing_total", "the order carries no total; a missing total is never collected as zero")
	ErrInvalidWebhook    = kernel.NewError(kernel.KindInvalidArgument, "payment.invalid_webhook", "the notification is not one a payment can be told about")
	ErrNotFound          = kernel.NewError(kernel.KindNotFound, "payment.not_found", "payment not found")
	ErrNotPending        = kernel.NewError(kernel.KindFailedPrecondition, "payment.not_pending", "the payment is no longer pending")
	ErrExpired           = kernel.NewError(kernel.KindFailedPrecondition, "payment.expired", "the payment window has closed")
	ErrNotRefunding      = kernel.NewError(kernel.KindFailedPrecondition, "payment.not_refunding", "the payment is not being refunded")
	ErrPermissionDenied  = kernel.NewError(kernel.KindPermissionDenied, "payment.permission_denied", "an admin cannot pay for another customer's order")
	ErrDuplicateForOrder = kernel.NewError(kernel.KindConflict, "payment.already_exists", "the order already has a payment")

	// ErrStale is returned by Repository.Save when the row changed since it was loaded (optimistic
	// concurrency). It wraps kernel.ErrConcurrentUpdate and is deliberately NOT a kernel.Error: a
	// lost race must be retried, not reported as a final domain failure.
	ErrStale = fmt.Errorf("payment: reload and retry: %w", kernel.ErrConcurrentUpdate)
)
