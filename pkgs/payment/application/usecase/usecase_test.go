package usecase_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
)

var t0 = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)

const (
	owner    = "cust-owner"
	stranger = "cust-stranger"
	orderID  = "ord-1"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type env struct {
	repo  *memory.PaymentRepository
	prov  *mock.Provider
	clock *clock
	start *usecase.StartPayment
	get   *usecase.GetPayment
	// asked counts what reached the provider, by operation, through Options.Fault (which sees every call).
	mu    sync.Mutex
	asked map[string]int
}

func (e *env) calls(op string) int {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.asked[op]
}

func newEnv(t *testing.T, fault func(op string) error) *env {
	t.Helper()
	e := &env{repo: memory.NewPaymentRepository(), clock: &clock{now: t0}, asked: map[string]int{}}
	e.prov = mock.New(mock.Options{
		CheckoutTTL: 30 * time.Minute, Now: e.clock.Now,
		Fault: func(op string) error {
			e.mu.Lock()
			e.asked[op]++
			e.mu.Unlock()
			if fault != nil {
				return fault(op)
			}

			return nil
		},
	})
	e.start = usecase.NewStartPayment(e.repo, e.prov, memory.NewTransactor(), e.clock)
	e.get = usecase.NewGetPayment(e.repo)

	return e
}

func money(t *testing.T) kernel.Money {
	t.Helper()
	m, err := kernel.NewMoney(1000, "TWD")
	if err != nil {
		t.Fatal(err)
	}

	return m
}

// seed stores a PENDING payment of owner's order, window open for 30 minutes (S2 has no T1: the
// consumer that creates payments is slice 3, so a payment comes from the repository).
func (e *env) seed(t *testing.T) {
	t.Helper()
	p, err := payment.New(payment.NewParams{ID: "pay-1", OrderID: orderID, CustomerID: owner, Total: money(t), ExpiresAt: t0.Add(30 * time.Minute)}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.repo.Save(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func startIn(customer string, admin bool) dto.StartPaymentInput {
	return dto.StartPaymentInput{OrderID: orderID, CustomerID: customer, Admin: admin}
}

// seenCheckouts records what StartPayment asks of the provider.
type seenCheckouts struct {
	port.PaymentProvider
	asked []dto.ProviderCheckoutInput
}

func (s *seenCheckouts) CreateCheckout(c context.Context, in dto.ProviderCheckoutInput) (dto.ProviderCheckoutOutput, error) {
	s.asked = append(s.asked, in)

	return s.PaymentProvider.CreateCheckout(c, in)
}

// T2: one payment has ONE checkout for its whole window. It is opened to end with the window, and a later
// StartPayment hands out the same URL at any moment of the window, long after a shorter page would have
// ended: a second checkout could be paid too, and nothing defines how the second payment would be refunded.
func TestStartPayment_OneCheckoutForTheWholeWindow(t *testing.T) {
	e := newEnv(t, nil)
	e.seed(t)
	c := context.Background()
	seen := &seenCheckouts{PaymentProvider: e.prov}
	start := usecase.NewStartPayment(e.repo, seen, memory.NewTransactor(), e.clock)

	first, err := start.Execute(c, startIn(owner, false))
	if err != nil {
		t.Fatal(err)
	}
	if len(seen.asked) != 1 || !seen.asked[0].ExpiresAt.Equal(t0.Add(30*time.Minute)) {
		t.Fatalf("the provider was asked %+v, want one checkout ending with the window (+30m)", seen.asked)
	}

	for _, after := range []time.Duration{9 * time.Minute, 11 * time.Minute, 29 * time.Minute} {
		e.clock.Advance(after - e.clock.Now().Sub(t0))
		again, err := start.Execute(c, startIn(owner, false))
		if err != nil || again.CheckoutURL != first.CheckoutURL || len(seen.asked) != 1 {
			t.Fatalf("at +%s: url %q (first %q), %d checkouts asked, err %v; want the same URL and still 1 checkout", after, again.CheckoutURL, first.CheckoutURL, len(seen.asked), err)
		}
	}
}

func TestStartPayment_Refusals(t *testing.T) {
	t.Run("window closed: refused before the provider is asked", func(t *testing.T) {
		e := newEnv(t, nil)
		e.seed(t)
		e.clock.Advance(31 * time.Minute)
		if _, err := e.start.Execute(context.Background(), startIn(owner, false)); !errors.Is(err, payment.ErrExpired) {
			t.Fatalf("err = %v, want payment.expired", err)
		}
		if e.calls(mock.OpCreateCheckout) != 0 {
			t.Fatal("the provider was asked for a checkout of a payment past its window")
		}
	})

	t.Run("not pending: refused before the provider is asked", func(t *testing.T) {
		e := newEnv(t, nil)
		e.seed(t)
		p, _ := e.repo.FindByOrderID(context.Background(), orderID)
		p.Capture(payment.SourceWebhook, money(t), t0)
		if err := e.repo.Save(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		if _, err := e.start.Execute(context.Background(), startIn(owner, false)); !errors.Is(err, payment.ErrNotPending) {
			t.Fatalf("err = %v, want payment.not_pending", err)
		}
		if e.calls(mock.OpCreateCheckout) != 0 {
			t.Fatal("the provider was asked for a checkout of a captured payment")
		}
	})

	t.Run("an order without a payment", func(t *testing.T) {
		e := newEnv(t, nil)
		if _, err := e.start.Execute(context.Background(), startIn(owner, false)); !errors.Is(err, payment.ErrNotFound) {
			t.Fatalf("err = %v, want payment.not_found", err)
		}
	})

	t.Run("no order id", func(t *testing.T) {
		e := newEnv(t, nil)
		if _, err := e.start.Execute(context.Background(), dto.StartPaymentInput{CustomerID: owner}); !errors.Is(err, payment.ErrInvalidOrderID) {
			t.Fatalf("err = %v, want invalid order id", err)
		}
		if _, err := e.get.Execute(context.Background(), dto.GetPaymentInput{CustomerID: owner}); !errors.Is(err, payment.ErrInvalidOrderID) {
			t.Fatalf("get: err = %v, want invalid order id", err)
		}
	})
}

// Ownership (docs/payment.md §6): the customer reads and pays their own; another customer's payment
// is NOT_FOUND to read and to pay; an admin reads anyone's but cannot pay for another customer; an
// empty customer is refused, never a wildcard (neither for a stranger-looking admin nor anyone else).
func TestOwnership(t *testing.T) {
	c := context.Background()

	type cell struct {
		name      string
		customer  string
		admin     bool
		wantRead  error
		wantStart error
	}
	for _, tc := range []cell{
		{"owner", owner, false, nil, nil},
		{"owner who is also admin", owner, true, nil, nil},
		{"another customer", stranger, false, payment.ErrNotFound, payment.ErrNotFound},
		{"admin acting for nobody in particular", stranger, true, nil, payment.ErrPermissionDenied},
		{"no customer", "", false, payment.ErrEmptyCustomer, payment.ErrEmptyCustomer},
		{"no customer, admin", "", true, payment.ErrEmptyCustomer, payment.ErrEmptyCustomer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, nil)
			e.seed(t)

			got, err := e.get.Execute(c, dto.GetPaymentInput{OrderID: orderID, CustomerID: tc.customer, Admin: tc.admin})
			if !errors.Is(err, tc.wantRead) {
				t.Fatalf("read: err = %v, want %v", err, tc.wantRead)
			}
			if tc.wantRead == nil && (got == nil || got.OrderID != orderID || got.CustomerID != owner) {
				t.Fatalf("read: got %+v, want the owner's payment", got)
			}
			if tc.wantRead != nil && got != nil {
				t.Fatalf("read refused but returned %+v", got)
			}

			_, err = e.start.Execute(c, startIn(tc.customer, tc.admin))
			if !errors.Is(err, tc.wantStart) {
				t.Fatalf("start: err = %v, want %v", err, tc.wantStart)
			}
			wantCalls := 0
			if tc.wantStart == nil {
				wantCalls = 1
			}
			if e.calls(mock.OpCreateCheckout) != wantCalls {
				t.Fatalf("start: CreateCheckout calls = %d, want %d (a refused caller must not cost the provider a call)", e.calls(mock.OpCreateCheckout), wantCalls)
			}
		})
	}
}

// A provider that cannot answer leaves the payment as it was and the caller with a fixed error that
// carries none of the provider's words (#347); the retry works once the provider is back.
func TestStartPayment_ProviderDown(t *testing.T) {
	const marker = "SECRET-MARKER-3f9a"
	down := true
	e := newEnv(t, func(string) error {
		if down {
			return errors.New("provider says: bad credentials " + marker)
		}

		return nil
	})
	e.seed(t)
	c := context.Background()

	_, err := e.start.Execute(c, startIn(owner, false))
	if !errors.Is(err, port.ErrProviderUnavailable) {
		t.Fatalf("err = %v, want the provider-unavailable sentinel", err)
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("the provider's words reached the caller: %v", err)
	}
	if e.calls(mock.OpCreateCheckout) != 1 {
		t.Fatalf("positive control: the provider was asked %d times, want 1 (the marker never reached the fake otherwise)", e.calls(mock.OpCreateCheckout))
	}
	got, _ := e.get.Execute(c, dto.GetPaymentInput{OrderID: orderID, CustomerID: owner})
	if got.CheckoutURL != "" || got.Status != "PENDING" {
		t.Fatalf("payment changed by a failed start: %+v", got)
	}

	down = false
	if out, err := e.start.Execute(c, startIn(owner, false)); err != nil || out.CheckoutURL == "" {
		t.Fatalf("after recovery: %+v, %v", out, err)
	}
}

// reentrant lets a test run a second StartPayment while the first one is inside the provider call, so
// the lost race is deterministic.
type reentrant struct {
	*mock.Provider
	onCreate func()
}

func (r *reentrant) CreateCheckout(c context.Context, req dto.ProviderCheckoutInput) (dto.ProviderCheckoutOutput, error) {
	co, err := r.Provider.CreateCheckout(c, req)
	if r.onCreate != nil {
		f := r.onCreate
		r.onCreate = nil
		f()
	}

	return co, err
}

// Two StartPayments of the same payment at once: one URL wins and both callers get it; the page the
// loser opened is cancelled, not left dangling at the provider.
func TestStartPayment_LostRaceReturnsTheWinnersCheckout(t *testing.T) {
	e := newEnv(t, nil)
	e.seed(t)
	c := context.Background()
	re := &reentrant{Provider: e.prov}
	start := usecase.NewStartPayment(e.repo, re, memory.NewTransactor(), e.clock)

	var winner *dto.PaymentOutput
	re.onCreate = func() {
		var err error
		if winner, err = e.start.Execute(c, startIn(owner, false)); err != nil { // the "other request"
			t.Errorf("winner: %v", err)
		}
	}
	loser, err := start.Execute(c, startIn(owner, false))
	if err != nil {
		t.Fatal(err)
	}
	if winner == nil || loser.CheckoutURL != winner.CheckoutURL {
		t.Fatalf("loser got %q, winner %+v; want one URL", loser.CheckoutURL, winner)
	}
	if e.prov.Opened() != 2 {
		t.Fatalf("opened %d checkouts, want 2 (one each)", e.prov.Opened())
	}
	// the loser opened its page first (mock_co_1), then the winner ran inside that call and opened mock_co_2
	if !e.prov.Cancelled("mock_co_1") || e.prov.Cancelled("mock_co_2") {
		t.Fatalf("the loser's page (mock_co_1) must be cancelled and the winner's (mock_co_2) kept: cancelled 1=%v 2=%v", e.prov.Cancelled("mock_co_1"), e.prov.Cancelled("mock_co_2"))
	}
	if loser.CheckoutURL != "https://mock-pay.invalid/checkout/mock_co_2" {
		t.Fatalf("loser's URL %q is not the winner's page", loser.CheckoutURL)
	}
}

// The other writer moved the payment out of PENDING while this call was at the provider: the call is
// refused and the page it opened is cancelled.
func TestStartPayment_PaymentEndedDuringTheProviderCall(t *testing.T) {
	e := newEnv(t, nil)
	e.seed(t)
	c := context.Background()
	re := &reentrant{Provider: e.prov}
	start := usecase.NewStartPayment(e.repo, re, memory.NewTransactor(), e.clock)

	re.onCreate = func() {
		p, err := e.repo.FindByOrderID(c, orderID)
		if err != nil {
			t.Error(err)
			return
		}
		p.OrderCancelled(t0)
		if err = e.repo.Save(c, p); err != nil {
			t.Error(err)
		}
	}
	if _, err := start.Execute(c, startIn(owner, false)); !errors.Is(err, payment.ErrNotPending) {
		t.Fatalf("err = %v, want payment.not_pending", err)
	}
	if !e.prov.Cancelled("mock_co_1") {
		t.Fatal("the page opened for a payment that ended meanwhile was not cancelled")
	}
}

// A store that keeps losing: three attempts, then ErrStale (a retry is safe), and the page is dropped.
type staleRepo struct {
	payment.Repository
}

func (staleRepo) Save(context.Context, *payment.Payment) error { return payment.ErrStale }

func TestStartPayment_GivesUpAfterBoundedAttempts(t *testing.T) {
	e := newEnv(t, nil)
	e.seed(t)
	start := usecase.NewStartPayment(staleRepo{e.repo}, e.prov, memory.NewTransactor(), e.clock)
	_, err := start.Execute(context.Background(), startIn(owner, false))
	if !errors.Is(err, payment.ErrStale) {
		t.Fatalf("err = %v, want ErrStale", err)
	}
	if !e.prov.Cancelled("mock_co_1") {
		t.Fatal("the page of a start that never saved was not cancelled")
	}
}

type failingRepo struct {
	payment.Repository
	err error
}

func (f failingRepo) Save(context.Context, *payment.Payment) error { return f.err }

func TestStartPayment_InfrastructureFailureIsReportedAndDropsThePage(t *testing.T) {
	e := newEnv(t, nil)
	e.seed(t)
	boom := errors.New("connection reset")
	start := usecase.NewStartPayment(failingRepo{e.repo, boom}, e.prov, memory.NewTransactor(), e.clock)
	if _, err := start.Execute(context.Background(), startIn(owner, false)); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the store's error", err)
	}
	if !e.prov.Cancelled("mock_co_1") {
		t.Fatal("the page of a start that failed to save was not cancelled")
	}
}
