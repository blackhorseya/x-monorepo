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

func (i *PaymentRepository) FindByID(c context.Context, id payment.ID) (*payment.Payment, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	order, ok := i.ids[id]
	if !ok {
		return nil, payment.ErrNotFound
	}

	return payment.Reconstitute(i.data[order])
}

func (i *PaymentRepository) FindByProviderRef(c context.Context, provider, ref string) (*payment.Payment, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	if ref != "" {
		for _, rec := range i.data {
			if rec.Provider == provider && rec.ProviderRef == ref {
				return payment.Reconstitute(rec)
			}
		}
	}

	return nil, payment.ErrNotFound
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

// OutcomeRepository keeps the order outcomes of payment.OutcomeRepository in memory.
type OutcomeRepository struct {
	mu   sync.Mutex
	data map[string]payment.Outcome
}

func NewOutcomeRepository() *OutcomeRepository {
	return &OutcomeRepository{data: map[string]payment.Outcome{}}
}

func (i *OutcomeRepository) Find(c context.Context, orderID string) (payment.Outcome, error) {
	if err := contextx.FromContext(c).Err(); err != nil {
		return payment.OutcomeNone, err
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	if o, ok := i.data[orderID]; ok {
		return o, nil
	}

	return payment.OutcomeNone, nil
}

func (i *OutcomeRepository) Record(c context.Context, orderID string, outcome payment.Outcome, _ time.Time) error {
	if err := contextx.FromContext(c).Err(); err != nil {
		return err
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	if _, ok := i.data[orderID]; !ok {
		i.data[orderID] = outcome
	}

	return nil
}

var _ payment.WebhookRepository = (*WebhookRepository)(nil)

// WebhookRepository keeps the verified notifications. SPEC STAGE: it keeps nothing.
type WebhookRepository struct{}

func NewWebhookRepository() *WebhookRepository { return &WebhookRepository{} }

func (i *WebhookRepository) Insert(context.Context, *payment.WebhookEvent) (bool, error) {
	return true, nil
}

func (i *WebhookRepository) Find(context.Context, string, string) (*payment.WebhookEvent, error) {
	return nil, payment.ErrNotFound
}

func (i *WebhookRepository) MarkProcessed(context.Context, string, string, time.Time) error {
	return nil
}

func (i *WebhookRepository) DeleteProcessedBefore(context.Context, time.Time, int) (int, error) {
	return 0, nil
}
