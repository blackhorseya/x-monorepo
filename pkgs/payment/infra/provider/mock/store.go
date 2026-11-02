package mock

import (
	"context"
	"sync"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
)

// Checkout is what the mock remembers about one hosted checkout.
type Checkout struct {
	PaymentID string
	Amount    dto.MoneyOutput
	URL       string
	ExpiresAt time.Time
	Paid      bool
	PaidWith  dto.MoneyOutput // what the customer paid; meaningful when Paid
	Cancelled bool
}

// Store is where a Provider keeps its checkouts, payments and refunds. Every method is one atomic
// step that the store itself guarantees (a mutex in memory, one SQL statement in Postgres), so two
// providers over one store — three processes over one database — never need a lock of their own.
// found is false for a ref the store has never seen; an error means the store could not answer.
type Store interface {
	// Create records a new checkout under a ref that is not in use.
	Create(c context.Context, ref string, co Checkout) error
	// Get reads a checkout.
	Get(c context.Context, ref string) (co Checkout, found bool, err error)
	// MarkPaid records that the customer paid with: found is false if there is no such checkout. Paying
	// again replaces what was paid.
	MarkPaid(c context.Context, ref string, with dto.MoneyOutput) (found bool, err error)
	// MarkCancelled records that the checkout was cancelled.
	MarkCancelled(c context.Context, ref string) (found bool, err error)
	// Refund honours the idempotency key once for a PAID checkout; ok is false when there is no such
	// checkout or nobody paid it (nothing is recorded then). The same key again is ok and adds nothing.
	Refund(c context.Context, ref, key string) (ok bool, err error)
	// Refunds counts the distinct keys the checkout was refunded with.
	Refunds(c context.Context, ref string) (int, error)
	// Refs lists every checkout's ref, oldest first.
	Refs(c context.Context) ([]string, error)
	// SetFault makes the operation (one of AllOps) fail for every provider over this store (on) or stops it
	// (off); setting what is already set is a success.
	SetFault(c context.Context, op string, on bool) error
	// Faulted reports whether the operation is set to fail.
	Faulted(c context.Context, op string) (bool, error)
}

// MemoryStore keeps everything in this process: the default, and what `task run` (no database) uses.
type MemoryStore struct {
	mu        sync.Mutex
	refs      []string
	checkouts map[string]*memCheckout
	faults    map[string]struct{}
}

type memCheckout struct {
	Checkout
	refunded map[string]struct{}
}

var _ Store = (*MemoryStore)(nil)

// NewMemoryStore creates an empty in-process store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{checkouts: map[string]*memCheckout{}, faults: map[string]struct{}{}}
}

func (s *MemoryStore) Create(_ context.Context, ref string, co Checkout) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkouts[ref] = &memCheckout{Checkout: co, refunded: map[string]struct{}{}}
	s.refs = append(s.refs, ref)

	return nil
}

func (s *MemoryStore) Get(_ context.Context, ref string) (Checkout, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	co, ok := s.checkouts[ref]
	if !ok {
		return Checkout{}, false, nil
	}

	return co.Checkout, true, nil
}

func (s *MemoryStore) MarkPaid(_ context.Context, ref string, with dto.MoneyOutput) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	co, ok := s.checkouts[ref]
	if !ok {
		return false, nil
	}
	co.Paid, co.PaidWith = true, with

	return true, nil
}

func (s *MemoryStore) MarkCancelled(_ context.Context, ref string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	co, ok := s.checkouts[ref]
	if !ok {
		return false, nil
	}
	co.Cancelled = true

	return true, nil
}

func (s *MemoryStore) Refund(_ context.Context, ref, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	co, ok := s.checkouts[ref]
	if !ok || !co.Paid {
		return false, nil
	}
	co.refunded[key] = struct{}{}

	return true, nil
}

func (s *MemoryStore) Refunds(_ context.Context, ref string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	co, ok := s.checkouts[ref]
	if !ok {
		return 0, nil
	}

	return len(co.refunded), nil
}

func (s *MemoryStore) Refs(context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.refs...), nil
}

func (s *MemoryStore) SetFault(_ context.Context, op string, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if on {
		s.faults[op] = struct{}{}
	} else {
		delete(s.faults, op)
	}

	return nil
}

func (s *MemoryStore) Faulted(_ context.Context, op string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.faults[op]

	return ok, nil
}
