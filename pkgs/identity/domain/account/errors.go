package account

import (
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

var (
	ErrInvalidID       = kernel.NewError(kernel.KindInvalidArgument, "account.invalid_id", "account id must not be empty")
	ErrInvalidName     = kernel.NewError(kernel.KindInvalidArgument, "account.invalid_name", "account name must not be empty")
	ErrInvalidRole     = kernel.NewError(kernel.KindInvalidArgument, "account.invalid_role", "a role must not be empty")
	ErrInvalidCursor   = kernel.NewError(kernel.KindInvalidArgument, "account.invalid_cursor", "page token does not continue a listing")
	ErrAlreadyDisabled = kernel.NewError(kernel.KindFailedPrecondition, "account.already_disabled", "account is already disabled")
	ErrAlreadyActive   = kernel.NewError(kernel.KindFailedPrecondition, "account.already_active", "account is already active")
	// ErrDisabled is why a credential of a disabled account does not resolve.
	ErrDisabled  = kernel.NewError(kernel.KindFailedPrecondition, "account.disabled", "account is disabled")
	ErrNotFound  = kernel.NewError(kernel.KindNotFound, "account.not_found", "account not found")
	ErrDuplicate = kernel.NewError(kernel.KindConflict, "account.duplicate", "account already exists")
	// ErrInvalidStatus means persisted state names a status this code does not know.
	ErrInvalidStatus = kernel.NewError(kernel.KindInternal, "account.invalid_status", "persisted account status is unknown")

	// ErrStale is returned by Repository.Save when the row changed since it was
	// loaded (optimistic concurrency). It wraps kernel.ErrConcurrentUpdate and is
	// deliberately NOT a kernel.Error: rpc answers Aborted and the client retries.
	ErrStale = fmt.Errorf("account: reload and retry: %w", kernel.ErrConcurrentUpdate)
)
