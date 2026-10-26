package apikey

import (
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

var (
	ErrInvalidID        = kernel.NewError(kernel.KindInvalidArgument, "api_key.invalid_id", "api key id must not be empty")
	ErrInvalidAccountID = kernel.NewError(kernel.KindInvalidArgument, "api_key.invalid_account_id", "api key must belong to an account")
	ErrInvalidName      = kernel.NewError(kernel.KindInvalidArgument, "api_key.invalid_name", "api key name must not be empty")
	ErrInvalidSecret    = kernel.NewError(kernel.KindInvalidArgument, "api_key.invalid_secret", "api key secret is too short")
	ErrInvalidExpiry    = kernel.NewError(kernel.KindInvalidArgument, "api_key.invalid_expiry", "api key expiry must be in the future")
	ErrAlreadyRevoked   = kernel.NewError(kernel.KindFailedPrecondition, "api_key.already_revoked", "api key is already revoked")
	// ErrRevoked / ErrExpired are why a presented key does not resolve.
	ErrRevoked   = kernel.NewError(kernel.KindFailedPrecondition, "api_key.revoked", "api key is revoked")
	ErrExpired   = kernel.NewError(kernel.KindFailedPrecondition, "api_key.expired", "api key is expired")
	ErrNotFound  = kernel.NewError(kernel.KindNotFound, "api_key.not_found", "api key not found")
	ErrDuplicate = kernel.NewError(kernel.KindConflict, "api_key.duplicate", "api key already exists")
	// ErrInvalidStatus means persisted state names a status this code does not know.
	ErrInvalidStatus = kernel.NewError(kernel.KindInternal, "api_key.invalid_status", "persisted api key status is unknown")

	// ErrStale is returned by Repository.Save when the row changed since it was
	// loaded (optimistic concurrency). It wraps kernel.ErrConcurrentUpdate and is
	// deliberately NOT a kernel.Error: rpc answers Aborted and the client retries.
	ErrStale = fmt.Errorf("api key: reload and retry: %w", kernel.ErrConcurrentUpdate)
)
