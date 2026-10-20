package server_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// peerRequest is a hand-built request with a given peer address.
type peerRequest struct {
	connect.AnyRequest
	addr string
}

func (r peerRequest) Peer() connect.Peer {
	return connect.Peer{Addr: r.addr, Protocol: connect.ProtocolConnect}
}

func errorCode(err error) (connect.Code, string, string) {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return 0, "", ""
	}

	return ce.Code(), ce.Meta().Get("x-error-code"), ce.Meta().Get(server.RetryAfterHeader)
}

func TestTimeoutInterceptor(t *testing.T) {
	if server.NewTimeoutInterceptor(0) != nil {
		t.Fatal("timeout 0 must be off")
	}
	in := server.NewTimeoutInterceptor(50 * time.Millisecond)

	// The use case outlives the deadline and surfaces the driver's error
	// through ToConnectError (Internal): the client still learns it was a timeout.
	slow := in(func(c context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		<-c.Done()
		return nil, server.ToConnectError(fmt.Errorf("query orders: %w", c.Err()))
	})
	_, err := slow(context.Background(), connect.NewRequest(&struct{}{}))
	if code, ec, _ := errorCode(err); code != connect.CodeDeadlineExceeded || ec != server.TimeoutCode {
		t.Fatalf("slow = %v (code %s, x-error-code %q), want DeadlineExceeded/%s", err, code, ec, server.TimeoutCode)
	}

	// Success after the deadline is still success: the work was committed.
	late := in(func(c context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		<-c.Done()
		return connect.NewResponse(&struct{}{}), nil
	})
	if _, err := late(context.Background(), connect.NewRequest(&struct{}{})); err != nil {
		t.Fatalf("late success = %v, want nil", err)
	}

	// A failure within the deadline is untouched.
	fast := in(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("nope"))
	})
	_, err = fast(context.Background(), connect.NewRequest(&struct{}{}))
	if code, _, _ := errorCode(err); code != connect.CodeNotFound {
		t.Fatalf("fast failure = %v, want NotFound", err)
	}

	// The client's shorter deadline is reported the same way.
	c, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err = slow(c, connect.NewRequest(&struct{}{}))
	if code, ec, _ := errorCode(err); code != connect.CodeDeadlineExceeded || ec != server.TimeoutCode {
		t.Fatalf("client deadline = %v, want DeadlineExceeded/%s", err, server.TimeoutCode)
	}
}

func TestRateLimitInterceptor(t *testing.T) {
	if in, err := server.NewRateLimitInterceptor(config.RateLimit{}); err != nil || in != nil {
		t.Fatalf("rps 0 must be off: %v, %v", in, err)
	}
	if _, err := server.NewRateLimitInterceptor(config.RateLimit{RPS: 1, By: "user"}); err == nil {
		t.Fatal("unknown by must fail")
	}
	ok := func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		return connect.NewResponse(&struct{}{}), nil
	}
	call := func(next connect.UnaryFunc, addr string) error {
		_, err := next(context.Background(), peerRequest{AnyRequest: connect.NewRequest(&struct{}{}), addr: addr})
		return err
	}

	t.Run("global", func(t *testing.T) {
		in, err := server.NewRateLimitInterceptor(config.RateLimit{RPS: 1, Burst: 2})
		if err != nil {
			t.Fatal(err)
		}
		next := in(ok)
		for i := range 2 {
			if err := call(next, "10.0.0.1:1"); err != nil {
				t.Fatalf("call %d within burst = %v", i, err)
			}
		}
		err = call(next, "10.0.0.2:1") // another peer: same bucket
		code, ec, retry := errorCode(err)
		if code != connect.CodeResourceExhausted || ec != server.RateLimitedCode || retry != "1" {
			t.Fatalf("third call = %v (code %s, x-error-code %q, Retry-After %q)", err, code, ec, retry)
		}
	})

	t.Run("peer", func(t *testing.T) {
		in, err := server.NewRateLimitInterceptor(config.RateLimit{RPS: 1, Burst: 1, By: config.RateLimitPeer})
		if err != nil {
			t.Fatal(err)
		}
		next := in(ok)
		// Ports differ per connection; the host is the key.
		if err := call(next, "10.0.0.1:1111"); err != nil {
			t.Fatal(err)
		}
		if code, _, _ := errorCode(call(next, "10.0.0.1:2222")); code != connect.CodeResourceExhausted {
			t.Fatalf("same host, new port: code %s, want ResourceExhausted", code)
		}
		if err := call(next, "10.0.0.2:1111"); err != nil {
			t.Fatalf("other host has its own bucket: %v", err)
		}
	})

	t.Run("burst defaults to rps", func(t *testing.T) {
		in, err := server.NewRateLimitInterceptor(config.RateLimit{RPS: 2.5})
		if err != nil {
			t.Fatal(err)
		}
		next := in(ok)
		for i := range 3 {
			if err := call(next, "h:1"); err != nil {
				t.Fatalf("call %d: %v", i, err)
			}
		}
		if code, _, _ := errorCode(call(next, "h:1")); code != connect.CodeResourceExhausted {
			t.Fatalf("4th call code %s, want ResourceExhausted", code)
		}
	})
}
