// Package kernel is the shared kernel: value objects and error semantics every
// bounded context agrees on. It depends on nothing (no proto, no BC, no platform).
package kernel

import "fmt"

// Kind classifies a domain error so adapters can map it to a transport code
// without the domain knowing about gRPC / HTTP / MQ.
type Kind uint8

const (
	KindInternal Kind = iota
	KindInvalidArgument
	KindNotFound
	KindFailedPrecondition
	KindConflict
)

func (k Kind) String() string {
	switch k {
	case KindInvalidArgument:
		return "invalid_argument"
	case KindNotFound:
		return "not_found"
	case KindFailedPrecondition:
		return "failed_precondition"
	case KindConflict:
		return "conflict"
	default:
		return "internal"
	}
}

// Error is a domain error with a stable machine-readable code.
type Error struct {
	kind Kind
	code string
	msg  string
}

// NewError creates a domain error. code should be stable, e.g. "order.not_found".
func NewError(kind Kind, code, msg string) *Error {
	return &Error{kind: kind, code: code, msg: msg}
}

func (x *Error) Error() string   { return fmt.Sprintf("%s: %s", x.code, x.msg) }
func (x *Error) Kind() Kind      { return x.kind }
func (x *Error) Code() string    { return x.code }
func (x *Error) Message() string { return x.msg }
