package server

import (
	"errors"
	"net/http"

	"connectrpc.com/connect/v2"
)

// connect v1 let an error carry response metadata (Error.Meta()): x-error-code,
// Retry-After, X-Request-Id. A v2 *connect.Error has no such field, and a header
// set on the call (CallInfo.ResponseHeader) is not tied to the error, so the
// metadata travels as a local-only cause of the error: WithMeta puts it there,
// the meta interceptor (meta.go) takes it out on the way out and writes it where
// v1 put an error's Meta(). It never reaches the wire as part of the message;
// only as the headers / trailers below.

// errMeta is the cause WithMeta attaches. Its text is never shown: a cause is
// not serialized, and the interceptors log the code, not the cause.
type errMeta struct{ header http.Header }

func (*errMeta) Error() string { return "error response metadata" }

// WithMeta returns ce carrying the given header key/value pairs, to be sent as
// response metadata when the error leaves the server (headers for a Connect
// unary RPC, trailers for the others, as v1 did). An odd trailing key is ignored.
// Call it once per error: the first carrier in the cause chain wins.
func WithMeta(ce *connect.Error, kv ...string) *connect.Error {
	h := make(http.Header, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}

	return ce.WithCause(&errMeta{header: h})
}

// ErrorMeta returns the metadata WithMeta attached to err (or to the *connect.Error
// inside it), nil when none. It is what a v1 caller read with Error.Meta().
func ErrorMeta(err error) http.Header {
	var m *errMeta
	if errors.As(err, &m) {
		return m.header
	}

	return nil
}
