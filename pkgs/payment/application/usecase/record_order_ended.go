package usecase

import (
	"context"
	"errors"
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// RecordOrderEnded applies OrderCancelled or OrderCompleted to the order's payment (docs/payment.md
// T7 / T7' / T10 / T12 / T13 / T15), inside one transaction (Transactional + Idempotent):
//
//   - no payment yet: the outcome is written down (a tombstone) so that the payment, when
//     OrderConfirmed finally arrives, is born ended (T14);
//   - a payment: the domain decides the new state; PaymentRefundRequested goes to the outbox in the
//     same transaction when money is held for an order that ended (T10).
//
// The provider is NOT called here: whether a checkout is to be cancelled comes back in the output,
// and the decorator that runs after the commit does it, best effort (a failure is repaired by the
// reconcile sweep, T8/T9).
type RecordOrderEnded struct {
	payments payment.Repository
	outcomes payment.OutcomeRepository
	events   port.EventPublisher
	clock    port.Clock
	outcome  payment.Outcome
}

// NewRecordOrderCancelled is the use case of ordering.v1.OrderCancelled.
func NewRecordOrderCancelled(payments payment.Repository, outcomes payment.OutcomeRepository, events port.EventPublisher, clock port.Clock) *RecordOrderEnded {
	return &RecordOrderEnded{payments: payments, outcomes: outcomes, events: events, clock: clock, outcome: payment.OutcomeCancelled}
}

// NewRecordOrderCompleted is the use case of ordering.v1.OrderCompleted.
func NewRecordOrderCompleted(payments payment.Repository, outcomes payment.OutcomeRepository, events port.EventPublisher, clock port.Clock) *RecordOrderEnded {
	return &RecordOrderEnded{payments: payments, outcomes: outcomes, events: events, clock: clock, outcome: payment.OutcomeCompletedUnpaid}
}

func (uc *RecordOrderEnded) Execute(c context.Context, in dto.OrderEndedInput) (*dto.OrderEndedOutput, error) {
	ctx := contextx.FromContext(c)
	if in.OrderID == "" {
		return nil, payment.ErrInvalidOrderID
	}
	now := uc.clock.Now()

	if err := uc.outcomes.Lock(ctx, in.OrderID); err != nil {
		return nil, fmt.Errorf("lock order %s: %w", in.OrderID, err)
	}
	p, err := uc.payments.FindByOrderID(ctx, in.OrderID)
	if errors.Is(err, payment.ErrNotFound) {
		if err = uc.outcomes.Record(ctx, in.OrderID, uc.outcome, now); err != nil {
			return nil, fmt.Errorf("record outcome of order %s: %w", in.OrderID, err)
		}

		return &dto.OrderEndedOutput{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find payment of order %s: %w", in.OrderID, err)
	}

	var eff payment.Effect
	if uc.outcome == payment.OutcomeCancelled {
		eff = p.OrderCancelled(now)
	} else {
		eff = p.OrderCompleted(now)
	}
	if err = uc.payments.Save(ctx, p); err != nil {
		return nil, fmt.Errorf("save payment of order %s: %w", in.OrderID, err)
	}
	if err = uc.events.Publish(ctx, p, eff.Publish(), now); err != nil {
		return nil, err
	}
	out := &dto.OrderEndedOutput{}
	if eff.CancelCheckout() {
		out.CancelCheckoutRef = p.ProviderRef()
	}

	return out, nil
}
