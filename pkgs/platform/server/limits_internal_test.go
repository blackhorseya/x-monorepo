package server

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
)

// v1's generated unary wrapper refused a call whose context was already done before it ran
// the handler; v2's does not. refuseExpired puts that back: after http.timeout (here 1ns,
// gone before the handler is reached) the use case must not run, and the answer is the
// timeout's, DEADLINE_EXCEEDED + request.timeout.
func TestRefuseExpired_HandlerDoesNotRunAfterTheDeadline(t *testing.T) {
	spec := connect.Spec{Procedure: "/test.v1.TestService/Ping", StreamType: connect.StreamTypeUnary}
	for name, tc := range map[string]struct {
		ics     []connect.ServerInterceptor
		ran     bool
		wantErr connect.Code
	}{
		"timeout 1ns, guard on":  {ics: []connect.ServerInterceptor{NewTimeoutInterceptor(time.Nanosecond), refuseExpired}, wantErr: connect.CodeDeadlineExceeded},
		"timeout 1m, guard on":   {ics: []connect.ServerInterceptor{NewTimeoutInterceptor(time.Minute), refuseExpired}, ran: true},
		"no timeout, guard only": {ics: []connect.ServerInterceptor{refuseExpired}, ran: true},
	} {
		t.Run(name, func(t *testing.T) {
			ran := false
			srv := connect.NewServer(tc.ics...)
			srv.Register(connect.Method{Spec: spec, Handler: func(context.Context, connect.Spec, connect.ServerStream) error {
				ran = true
				return nil
			}})
			err := srv.Call(context.Background(), spec.Procedure, &connect.CallInfo{}, nil)
			if ran != tc.ran {
				t.Fatalf("handler ran = %v, want %v (err %v)", ran, tc.ran, err)
			}
			if tc.wantErr != 0 && (connect.CodeOf(err) != tc.wantErr || ErrorMeta(err).Get("x-error-code") != TimeoutCode) {
				t.Fatalf("err = %v (x-error-code %q), want %s / %s", err, ErrorMeta(err).Get("x-error-code"), tc.wantErr, TimeoutCode)
			}
			if tc.wantErr == 0 && err != nil {
				t.Fatal(err)
			}
		})
	}

	t.Run("a cancelled context is refused too", func(t *testing.T) {
		c, cancel := context.WithCancel(context.Background())
		cancel()
		srv := connect.NewServer(refuseExpired)
		srv.Register(connect.Method{Spec: spec, Handler: func(context.Context, connect.Spec, connect.ServerStream) error {
			t.Fatal("handler ran for a cancelled call")
			return nil
		}})
		if err := srv.Call(c, spec.Procedure, &connect.CallInfo{}, nil); err == nil {
			t.Fatal("cancelled call must fail")
		}
	})
}
