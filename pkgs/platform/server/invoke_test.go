package server_test

import (
	"context"

	"connectrpc.com/connect/v2"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/server/servertest"
)

// invoke, invokeOne and handled are servertest.Invoke with the arguments each test needs.
func invoke(c context.Context, ics []connect.ServerInterceptor, procedure, peerAddr string, headers map[string]string, h connect.ServerFunc) (*connect.CallInfo, error) {
	return servertest.Invoke(c, ics, procedure, peerAddr, headers, h)
}

// invokeOne is invoke with a single interceptor, no peer address.
func invokeOne(c context.Context, in connect.ServerInterceptor, procedure string, headers map[string]string, h connect.ServerFunc) error {
	_, err := invoke(c, []connect.ServerInterceptor{in}, procedure, "", headers, h)

	return err
}

// handled returns a handler that succeeds after running f (nil for none).
func handled(f func(context.Context)) connect.ServerFunc {
	return func(c context.Context, _ connect.Spec, _ connect.ServerStream) error {
		if f != nil {
			f(c)
		}

		return nil
	}
}
