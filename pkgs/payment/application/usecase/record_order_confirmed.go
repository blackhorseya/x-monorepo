package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// RecordOrderConfirmed creates the payment of a CONFIRMED order (docs/payment.md T1), driven by the
// consumer of ordering.v1.OrderConfirmed inside one transaction (provider.go: Transactional +
// Idempotent). It never asks the provider: the checkout opens at StartPayment.
//
//   - the order already has a payment: the same event again, ignored (unique(order_id)).
//   - the event carries no total: refused with payment.missing_total and alerted, no payment row —
//     an order is never billed for a total nobody sent, and an existing payment is not touched.
//   - OrderCancelled / OrderCompleted for the order arrived first (T14/T15): the payment is born
//     CANCELLED with that outcome and no timer.
type RecordOrderConfirmed struct {
	payments payment.Repository
	outcomes payment.OutcomeRepository
	ids      port.IDGenerator
	clock    port.Clock
	alerts   port.Alerts
	window   time.Duration
}

func NewRecordOrderConfirmed(payments payment.Repository, outcomes payment.OutcomeRepository, ids port.IDGenerator, clock port.Clock, alerts port.Alerts, window time.Duration) *RecordOrderConfirmed {
	return &RecordOrderConfirmed{payments: payments, outcomes: outcomes, ids: ids, clock: clock, alerts: alerts, window: window}
}

func (uc *RecordOrderConfirmed) Execute(c context.Context, in dto.OrderConfirmedInput) (*dto.Ack, error) {
	ctx := contextx.FromContext(c)
	if in.OrderID == "" {
		return nil, payment.ErrInvalidOrderID
	}

	existing, err := uc.payments.FindByOrderID(ctx, in.OrderID)
	switch {
	case err == nil:
		eff, err := existing.DuplicateConfirmed(in.Total != nil)
		if a := eff.Alert(); a != payment.AlertNone {
			uc.alerts.Raise(ctx, a, in.OrderID)
		}

		return &dto.Ack{}, err
	case !errors.Is(err, payment.ErrNotFound):
		return nil, fmt.Errorf("find payment of order %s: %w", in.OrderID, err)
	}

	if in.Total == nil {
		uc.alerts.Raise(ctx, payment.AlertMissingTotal, in.OrderID)

		return nil, payment.ErrMissingTotal
	}
	total, err := kernel.NewMoney(in.Total.Amount, in.Total.Currency)
	if err != nil {
		return nil, payment.ErrInvalidAmount
	}
	outcome, err := uc.outcomes.Find(ctx, in.OrderID)
	if err != nil {
		return nil, fmt.Errorf("find outcome of order %s: %w", in.OrderID, err)
	}
	id, err := uc.ids.NewID()
	if err != nil {
		return nil, fmt.Errorf("new payment id: %w", err)
	}

	now := uc.clock.Now()
	params := payment.NewParams{ID: payment.ID(id), OrderID: in.OrderID, CustomerID: in.CustomerID, Total: total, ExpiresAt: now.Add(uc.window)}
	var p *payment.Payment
	if outcome != payment.OutcomeNone {
		p, err = payment.NewForEndedOrder(params, outcome, now)
	} else {
		p, err = payment.New(params, now)
	}
	if err != nil {
		return nil, err
	}
	if err = uc.payments.Save(ctx, p); err != nil {
		if errors.Is(err, payment.ErrDuplicateForOrder) {
			return &dto.Ack{}, nil // a concurrent consumer created it first: the same event again
		}

		return nil, fmt.Errorf("save payment of order %s: %w", in.OrderID, err)
	}

	return &dto.Ack{}, nil
}
