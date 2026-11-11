package server

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	"golang.org/x/time/rate"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

// x-error-codes of the limit interceptors.
const (
	// TimeoutCode: the RPC ran past http.timeout (or the client's own
	// deadline); whatever the use case did was rolled back by its context.
	TimeoutCode = "request.timeout"
	// RateLimitedCode: no token in the bucket; Retry-After says when.
	RateLimitedCode = "request.rate_limited"
	// RetryAfterHeader accompanies RateLimitedCode, in whole seconds (≥ 1).
	RetryAfterHeader = "Retry-After"
)

// NewTimeoutInterceptor bounds every RPC to timeout: the use case runs with
// a context that expires, so a slow query is cancelled rather than holding
// the server (and a shutdown drain) hostage. A use case that failed because
// the deadline passed answers DEADLINE_EXCEEDED + x-error-code
// request.timeout whatever error it surfaced with (a pgx error wrapping
// context.DeadlineExceeded would otherwise be mapped to INTERNAL by
// ToConnectError); one that still succeeded keeps its response — the work
// is committed. The client's own deadline (Connect-Timeout-Ms / grpc-timeout)
// is already on the context and wins when shorter. timeout ≤ 0 returns nil.
func NewTimeoutInterceptor(timeout time.Duration) connect.ServerInterceptor {
	if timeout <= 0 {
		return nil
	}

	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(c context.Context, spec connect.Spec, stream connect.ServerStream) error {
			ctx, cancel := context.WithTimeout(c, timeout)
			defer cancel()
			err := next(ctx, spec, stream)
			if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return WithMeta(connect.Errorf(connect.CodeDeadlineExceeded, "request exceeded its deadline (%s)", timeout), "x-error-code", TimeoutCode)
			}

			return err
		}
	}
}

// refuseExpired is the innermost interceptor: a call whose context is already done fails
// before the handler starts. v1's generated unary wrapper did this inside the interceptor
// chain (connect.NewUnaryHandler: `if err := ctx.Err(); err != nil { return nil, err }`); a v2
// generated handler does not, so without it a request that used up its deadline before the
// handler (the timeout above, a client deadline already past, a client that went away) would
// still run the use case. The timeout interceptor turns the error into DEADLINE_EXCEEDED +
// request.timeout; anything else leaves as v1 left it, a plain error the meta interceptor
// answers as Internal.
func refuseExpired(next connect.ServerFunc) connect.ServerFunc {
	return func(c context.Context, spec connect.Spec, stream connect.ServerStream) error {
		if err := c.Err(); err != nil {
			return err
		}

		return next(c, spec, stream)
	}
}

// NewRateLimitInterceptor rejects RPCs beyond cfg's token bucket with
// RESOURCE_EXHAUSTED + x-error-code request.rate_limited + Retry-After. It
// sits outside auth on purpose: a flood must be dropped before each request
// pays for a JWT signature check. cfg.RPS ≤ 0 returns nil (off).
func NewRateLimitInterceptor(cfg config.RateLimit) (connect.ServerInterceptor, error) {
	if cfg.RPS <= 0 {
		return nil, nil
	}
	burst := cfg.Burst
	if burst <= 0 {
		burst = int(math.Ceil(cfg.RPS))
	}
	var limiter func(peerAddr string) *rate.Limiter
	switch cfg.By {
	case config.RateLimitGlobal, "":
		one := rate.NewLimiter(rate.Limit(cfg.RPS), burst)
		limiter = func(string) *rate.Limiter { return one }
	case config.RateLimitPeer:
		buckets := newBuckets(rate.Limit(cfg.RPS), burst)
		limiter = func(peerAddr string) *rate.Limiter { return buckets.get(peerHost(peerAddr)) }
	default:
		return nil, fmt.Errorf("rate limit: unknown by %q (want global|peer)", cfg.By)
	}

	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(c context.Context, spec connect.Spec, stream connect.ServerStream) error {
			r := limiter(callInfo(c).PeerAddr).Reserve()
			if delay := r.Delay(); delay > 0 {
				r.Cancel() // not waiting: give the token back

				return WithMeta(connect.NewError(connect.CodeResourceExhausted, "rate limit exceeded"),
					"x-error-code", RateLimitedCode,
					RetryAfterHeader, strconv.Itoa(int(math.Ceil(delay.Seconds()))))
			}

			return next(c, spec, stream)
		}
	}, nil
}

// peerHost is the client IP of addr (the call's peer address) without the port,
// which changes per connection and would make every HTTP/1.1 connection its own bucket.
func peerHost(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}

	return addr
}

// buckets holds one limiter per key and forgets keys idle for over
// bucketIdle, swept on access at most once per bucketIdle (no goroutine).
type buckets struct {
	limit rate.Limit
	burst int

	mu        sync.Mutex
	m         map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	l    *rate.Limiter
	seen time.Time
}

const bucketIdle = time.Minute

func newBuckets(limit rate.Limit, burst int) *buckets {
	return &buckets{limit: limit, burst: burst, m: map[string]*bucket{}, lastSweep: time.Now()}
}

func (b *buckets) get(key string) *rate.Limiter {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if now.Sub(b.lastSweep) > bucketIdle {
		for k, v := range b.m {
			if now.Sub(v.seen) > bucketIdle {
				delete(b.m, k)
			}
		}
		b.lastSweep = now
	}
	x, ok := b.m[key]
	if !ok {
		x = &bucket{l: rate.NewLimiter(b.limit, b.burst)}
		b.m[key] = x
	}
	x.seen = now

	return x.l
}
