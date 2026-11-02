package usecase

import (
	"time"

	"context"
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// ApplyPaymentCaptured moves a CONFIRMED order that requires payment to PAID and on to COMPLETED
// in ONE transaction (OrderPaid, then OrderCompleted; inventory commits its reservation on the
// latter) — payment.v1.PaymentCaptured, docs/payment.md §2 and §8. Everything about whether and
// how is the order's own: its payment_required snapshot and status (order.ApplyPaymentCaptured);
// this use case never reads ordering.require_payment. Driven by the consumer adapter only.
//
// A refused event (order.ErrCannotPay for a CANCELLED order, order.ErrNotConfirmed, not found) is a
// domain rejection the consumer drops; payment refunds the money on its own side (T10).
type ApplyPaymentCaptured struct {
	repo   order.Repository
	events port.EventPublisher
	clock  port.Clock
}

func NewApplyPaymentCaptured(repo order.Repository, events port.EventPublisher, clock port.Clock) *ApplyPaymentCaptured {
	return &ApplyPaymentCaptured{repo: repo, events: events, clock: clock}
}

func (uc *ApplyPaymentCaptured) Execute(c context.Context, in dto.PaymentCapturedInput) (*dto.PaymentEventOutput, error) {
	return applyPaymentEvent(c, uc.repo, uc.events, in.OrderID, uc.clock.Now, (*order.Order).ApplyPaymentCaptured)
}

// ApplyPaymentExpired cancels a CONFIRMED order that requires payment whose payment window ran
// out (payment.v1.PaymentExpired, §8); inventory releases the stock on the OrderCancelled that
// raises. Driven by the consumer adapter only.
type ApplyPaymentExpired struct {
	repo   order.Repository
	events port.EventPublisher
	clock  port.Clock
}

func NewApplyPaymentExpired(repo order.Repository, events port.EventPublisher, clock port.Clock) *ApplyPaymentExpired {
	return &ApplyPaymentExpired{repo: repo, events: events, clock: clock}
}

func (uc *ApplyPaymentExpired) Execute(c context.Context, in dto.PaymentExpiredInput) (*dto.PaymentEventOutput, error) {
	return applyPaymentEvent(c, uc.repo, uc.events, in.OrderID, uc.clock.Now, (*order.Order).ApplyPaymentExpired)
}

// applyPaymentEvent is the load → decide → save → publish shared by the two events. The aggregate
// is reloaded on every optimistic-lock conflict, so the answer is re-decided on what the winner
// saved: a PaymentCaptured that lost to a CancelOrder is refused, not forced over it.
func applyPaymentEvent(c context.Context, repo order.Repository, events port.EventPublisher, orderID string, now func() time.Time, apply func(*order.Order, time.Time) (order.PaymentEffect, error)) (*dto.PaymentEventOutput, error) {
	ctx := contextx.FromContext(c)

	if orderID == "" {
		return nil, order.ErrInvalidID
	}

	var (
		o      *order.Order
		effect order.PaymentEffect
	)
	err := retryOnConflict(ctx, func() error {
		var err error
		if o, err = repo.FindByID(ctx, order.ID(orderID)); err != nil {
			return err
		}
		if effect, err = apply(o, now()); err != nil {
			return err
		}
		if effect != order.PaymentApplied {
			return nil // nothing to save
		}
		if err = repo.Save(ctx, o); err != nil {
			return fmt.Errorf("save order: %w", err)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}
	if err = events.Publish(ctx, o.PullEvents()...); err != nil {
		return nil, err
	}

	return &dto.PaymentEventOutput{Effect: effect.String()}, nil
}
