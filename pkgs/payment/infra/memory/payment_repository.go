// Package memory holds the in-memory repository for local dev and tests. It stores copies so
// callers can't mutate stored state by aliasing, and honours the same contract as infra/postgres.
package memory

import (
	"context"
	"sync"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

var _ payment.Repository = (*PaymentRepository)(nil)

type PaymentRepository struct {
	mu   sync.RWMutex
	data map[string]payment.ReconstituteParams // by order id
	ids  map[payment.ID]string                 // payment id → order id
}

func NewPaymentRepository() *PaymentRepository {
	return &PaymentRepository{data: map[string]payment.ReconstituteParams{}, ids: map[payment.ID]string{}}
}

func (i *PaymentRepository) FindByOrderID(c context.Context, orderID string) (*payment.Payment, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	rec, ok := i.data[orderID]
	if !ok {
		return nil, payment.ErrNotFound
	}

	return payment.Reconstitute(rec)
}

func (i *PaymentRepository) FindByID(context.Context, payment.ID) (*payment.Payment, error) {
	return nil, payment.ErrNotFound // SPEC STAGE
}

func (i *PaymentRepository) FindByProviderRef(context.Context, string, string) (*payment.Payment, error) {
	return nil, payment.ErrNotFound // SPEC STAGE
}

// Save inserts a never-saved payment (Version 0) with version 1, or replaces a loaded one when the
// stored version is still the loaded one. A second payment for an order, or a reused payment id, is
// ErrDuplicateForOrder; a version that moved on is ErrStale.
func (i *PaymentRepository) Save(c context.Context, p *payment.Payment) error {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return err
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	cur, exists := i.data[p.OrderID()]
	if p.Version() == 0 {
		if _, taken := i.ids[p.ID()]; exists || taken {
			return payment.ErrDuplicateForOrder
		}
	} else if !exists || cur.Version != p.Version() || cur.ID != p.ID() {
		return payment.ErrStale
	}

	i.data[p.OrderID()] = snapshot(p)
	i.ids[p.ID()] = p.OrderID()

	return nil
}

// snapshot is the stored form of p: its state with the version Save advances.
func snapshot(p *payment.Payment) payment.ReconstituteParams {
	return payment.ReconstituteParams{
		ID: p.ID(), OrderID: p.OrderID(), CustomerID: p.CustomerID(), Amount: p.Amount(), Status: p.Status(),
		ExpiresAt: p.ExpiresAt(), Provider: p.Provider(), ProviderRef: p.ProviderRef(), CheckoutURL: p.CheckoutURL(),
		CheckoutExpiresAt: p.CheckoutExpiresAt(), Outcome: p.Outcome(), TerminalAt: p.TerminalAt(),
		Version: p.Version() + 1, UpdatedAt: p.UpdatedAt(),
	}
}

var _ payment.OutcomeRepository = (*OutcomeRepository)(nil)

// OutcomeRepository keeps the order outcomes of payment.OutcomeRepository. SPEC STAGE: it remembers nothing.
type OutcomeRepository struct{}

func NewOutcomeRepository() *OutcomeRepository { return &OutcomeRepository{} }

func (i *OutcomeRepository) Find(context.Context, string) (payment.Outcome, error) {
	return payment.OutcomeNone, nil
}

func (i *OutcomeRepository) Record(context.Context, string, payment.Outcome, time.Time) error {
	return nil
}
