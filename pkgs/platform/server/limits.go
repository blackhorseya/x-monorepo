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

	"connectrpc.com/connect"
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
func NewTimeoutInterceptor(timeout time.Duration) connect.UnaryInterceptorFunc {
	if timeout <= 0 {
		return nil
	}

	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(c context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			ctx, cancel := context.WithTimeout(c, timeout)
			defer cancel()
			res, err := next(ctx, req)
			if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
				ce := connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf("request exceeded its deadline (%s)", timeout))
				ce.Meta().Set("x-error-code", TimeoutCode)

				return nil, ce
			}

			return res, err
		}
	}
}

// NewRateLimitInterceptor rejects RPCs beyond cfg's token bucket with
// RESOURCE_EXHAUSTED + x-error-code request.rate_limited + Retry-After. It
// sits outside auth on purpose: a flood must be dropped before each request
// pays for a JWT signature check. cfg.RPS ≤ 0 returns nil (off).
func NewRateLimitInterceptor(cfg config.RateLimit) (connect.UnaryInterceptorFunc, error) {
	if cfg.RPS <= 0 {
		return nil, nil
	}
	burst := cfg.Burst
	if burst <= 0 {
		burst = int(math.Ceil(cfg.RPS))
	}
	var limiter func(req connect.AnyRequest) *rate.Limiter
	switch cfg.By {
	case config.RateLimitGlobal, "":
		one := rate.NewLimiter(rate.Limit(cfg.RPS), burst)
		limiter = func(connect.AnyRequest) *rate.Limiter { return one }
	case config.RateLimitPeer:
		buckets := newBuckets(rate.Limit(cfg.RPS), burst)
		limiter = func(req connect.AnyRequest) *rate.Limiter { return buckets.get(peerHost(req)) }
	default:
		return nil, fmt.Errorf("rate limit: unknown by %q (want global|peer)", cfg.By)
	}

	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(c context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			r := limiter(req).Reserve()
			if delay := r.Delay(); delay > 0 {
				r.Cancel() // not waiting: give the token back
				ce := connect.NewError(connect.CodeResourceExhausted, errors.New("rate limit exceeded"))
				ce.Meta().Set("x-error-code", RateLimitedCode)
				ce.Meta().Set(RetryAfterHeader, strconv.Itoa(int(math.Ceil(delay.Seconds()))))

				return nil, ce
			}

			return next(c, req)
		}
	}, nil
}

// peerHost is the client IP of req: Peer().Addr without the port, which
// changes per connection and would make every HTTP/1.1 connection its own bucket.
func peerHost(req connect.AnyRequest) string {
	addr := req.Peer().Addr
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
