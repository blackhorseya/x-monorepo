// Package servertest is what a test that calls the RPC server through a connect v2
// client needs and v1 gave for free: a client's request header set in one place, and
// the response metadata of a failed call.
//
// v1 hung the response headers and trailers on the error (connect.Error.Meta()), so a
// test read x-error-code, Retry-After or X-Request-Id off the error it got back. In v2
// they are on the call's connect.CallInfo; WithErrorMeta copies them onto the error so
// the assertion stays "the error's metadata says X", whatever the protocol.
package servertest

import (
	"context"
	"errors"
	"io"
	"net/http"

	"connectrpc.com/connect/v2"
)

// Invoke runs h inside the interceptors the way connect.Server.Call does for one Connect
// unary RPC to procedure, with the given request headers and peer address, and returns
// the call's metadata (the response headers and trailers the interceptors set) with the
// error. v1 tests built a request by hand and called the wrapped function; a v2
// interceptor wraps a stream-level function, so the call goes through a real
// connect.Server, the entry connecthttp uses. h reads its request, if it wants it, from
// the nil stream's place: pass a handler that does not touch the stream.
func Invoke(c context.Context, ics []connect.ServerInterceptor, procedure, peerAddr string, headers map[string]string, h connect.ServerFunc) (*connect.CallInfo, error) {
	spec := connect.Spec{Procedure: procedure, StreamType: connect.StreamTypeUnary}
	srv := connect.NewServer(ics...)
	srv.Register(connect.Method{Spec: spec, Handler: h})
	info := &connect.CallInfo{Spec: spec, Protocol: "connect", PeerAddr: peerAddr}
	for k, v := range headers {
		info.RequestHeader().Set(k, v)
	}

	return info, srv.Call(c, procedure, info, nil)
}

// WithHeader is a client interceptor that sets one request header on every call.
func WithHeader(key, value string) connect.ClientInterceptor {
	return func(next connect.ClientFunc) connect.ClientFunc {
		return func(c context.Context, spec connect.Spec) (connect.ClientStream, error) {
			if info, ok := connect.CallInfoForClientContext(c); ok {
				info.RequestHeader().Set(key, value)
			}

			return next(c, spec)
		}
	}
}

// metaError is an error plus the response metadata of the call that failed with it.
type metaError struct {
	error
	meta http.Header
}

func (e *metaError) Unwrap() error { return e.error }

// WithErrorMeta is a client interceptor that attaches the call's response headers and
// trailers to the error it fails with; ErrorMeta reads them back. errors.As still finds
// the *connect.Error underneath.
func WithErrorMeta() connect.ClientInterceptor {
	return func(next connect.ClientFunc) connect.ClientFunc {
		return func(c context.Context, spec connect.Spec) (connect.ClientStream, error) {
			stream, err := next(c, spec)
			if err != nil {
				return nil, err
			}
			info, _ := connect.CallInfoForClientContext(c)

			return &metaStream{ClientStream: stream, info: info}, nil
		}
	}
}

type metaStream struct {
	connect.ClientStream
	info *connect.CallInfo
}

func (s *metaStream) Receive(msg any) error { return s.attach(s.ClientStream.Receive(msg)) }
func (s *metaStream) Close() error          { return s.attach(s.ClientStream.Close()) }

func (s *metaStream) attach(err error) error {
	if err == nil || errors.Is(err, io.EOF) || s.info == nil {
		return err
	}
	meta := http.Header{}
	for k, vs := range s.info.ResponseHeader().All() {
		meta[k] = append(meta[k], vs...)
	}
	for k, vs := range s.info.ResponseTrailer().All() {
		meta[k] = append(meta[k], vs...)
	}

	return &metaError{error: err, meta: meta}
}

// ErrorMeta returns the response headers and trailers WithErrorMeta attached to err,
// an empty header when there are none.
func ErrorMeta(err error) http.Header {
	var m *metaError
	if errors.As(err, &m) {
		return m.meta
	}

	return http.Header{}
}
