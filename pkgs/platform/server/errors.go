package server

import (
	"errors"

	"connectrpc.com/connect"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

// ToConnectError maps kernel error kinds to Connect codes. Unknown errors become
// Internal and their message is hidden from clients. Shared by every BC's rpc adapter.
func ToConnectError(err error) error {
	var de *kernel.Error
	if !errors.As(err, &de) {
		return connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	code := connect.CodeInternal
	switch de.Kind() {
	case kernel.KindInvalidArgument:
		code = connect.CodeInvalidArgument
	case kernel.KindNotFound:
		code = connect.CodeNotFound
	case kernel.KindFailedPrecondition:
		code = connect.CodeFailedPrecondition
	case kernel.KindConflict:
		code = connect.CodeAlreadyExists
	}

	ce := connect.NewError(code, errors.New(de.Message()))
	ce.Meta().Set("x-error-code", de.Code())

	return ce
}
