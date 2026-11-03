package usecase_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// secretText stands for what a provider (or the driver under it) may say about a failure: untrusted
// text that can reflect a credential (#347). Every failure below carries it, and none of it may reach
// the log: the log keeps a fixed class (docs/payment.md §9 診斷).
const secretText = "sk_live_SECRET-reflected-by-the-provider"

// syncBuffer is a log sink that is safe for the slog handler's goroutines and the test's reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.String()
}

// records parses the JSON log lines written so far.
func (s *syncBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(s.String()), "\n") {
		if line == "" {
			continue
		}
		rec := map[string]any{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		out = append(out, rec)
	}

	return out
}

// failingStore is a mock.Store whose Create fails as the test says; everything else is the in-memory one.
type failingStore struct {
	*mock.MemoryStore
	create func(c context.Context) error
}

func (s *failingStore) Create(c context.Context, ref string, co mock.Checkout) error {
	return s.create(c)
}

// providerFailureLog: StartPayment is the call that stalled in #416 (a deadline_exceeded with nothing in
// the log to say whether it was the provider's fault, the store's or the clock's). When the provider
// cannot be reached the log says which of three FIXED classes it was, for which call, for which order —
// and nothing the provider said.
func TestStartPayment_ProviderFailureLogsAFixedClass(t *testing.T) {
	deadline := func(t *testing.T) (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 30*time.Millisecond)
	}
	live := func(t *testing.T) (context.Context, context.CancelFunc) {
		return context.WithCancel(context.Background())
	}

	cases := []struct {
		name string
		ctx  func(*testing.T) (context.Context, context.CancelFunc)
		opts func() mock.Options
		want string
	}{
		{
			name: "the provider is slower than the caller's deadline (injected latency)",
			ctx:  deadline,
			opts: func() mock.Options { return mock.Options{Latency: time.Hour} },
			want: "deadline",
		},
		{
			// The shape of #416: the call reaches the store and waits there until the caller's deadline;
			// the driver's own words ("canceling statement …") are not a context error, only the context says so.
			name: "the store is still working when the caller's deadline ends (driver text, no wrapped context error)",
			ctx:  deadline,
			opts: func() mock.Options {
				return mock.Options{Store: &failingStore{MemoryStore: mock.NewMemoryStore(), create: func(c context.Context) error {
					<-c.Done()

					return errors.New("ERROR: canceling statement due to user request " + secretText)
				}}}
			},
			want: "deadline",
		},
		{
			name: "an injected fault",
			ctx:  live,
			opts: func() mock.Options {
				return mock.Options{Fault: func(string) error { return errors.New("card processor said no " + secretText) }}
			},
			want: "fault",
		},
		{
			name: "a fault set in the store (payment-rpc mock fail)",
			ctx:  live,
			opts: func() mock.Options {
				st := mock.NewMemoryStore()
				_ = st.SetFault(context.Background(), mock.OpCreateCheckout, true)

				return mock.Options{Store: st, FaultsFromStore: true}
			},
			want: "fault",
		},
		{
			name: "the store fails for any other reason",
			ctx:  live,
			opts: func() mock.Options {
				return mock.Options{Store: &failingStore{MemoryStore: mock.NewMemoryStore(), create: func(context.Context) error {
					return errors.New("connection refused " + secretText)
				}}}
			},
			want: "other",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &syncBuffer{}
			opts := tc.opts()
			opts.Now = func() time.Time { return t0 }
			e := &env{repo: memory.NewPaymentRepository(), clock: &clock{now: t0}}
			e.prov = mock.New(opts)
			e.seed(t)
			start := usecase.NewStartPayment(e.repo, e.prov, memory.NewTransactor(), e.clock)

			c, cancel := tc.ctx(t)
			defer cancel()
			c = contextx.WithLogger(c, slog.New(slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})))

			_, err := start.Execute(c, startIn(owner, false))
			if !errors.Is(err, port.ErrProviderUnavailable) || err.Error() != port.ErrProviderUnavailable.Error() {
				t.Fatalf("err = %v, want exactly %v", err, port.ErrProviderUnavailable)
			}

			// the anchor of THIS call: the one record about its provider failure, for its order
			var failed []map[string]any
			for _, rec := range sink.records(t) {
				if rec["msg"] == "payment provider failed" && rec["order_id"] == orderID {
					failed = append(failed, rec)
				}
			}
			if len(failed) != 1 {
				t.Fatalf("%d \"payment provider failed\" records for %s, want exactly 1; log:\n%s", len(failed), orderID, sink.String())
			}
			if failed[0]["op"] != mock.OpCreateCheckout || failed[0]["class"] != tc.want {
				t.Fatalf("record = %v, want op=%s class=%s", failed[0], mock.OpCreateCheckout, tc.want)
			}
			if strings.Contains(sink.String(), secretText) {
				t.Fatalf("the provider's text reached the log:\n%s", sink.String())
			}
		})
	}
}
