package usecase

import (
	"context"
	"errors"
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/mapper"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// ProcessRefund makes the refund of a REFUNDING payment (docs/payment.md T11), driven by the consumer
// of payment.v1.PaymentRefundRequested.
//
// The provider is called OUTSIDE any database transaction (the third use case of the §1 exception
// table, next to StartPayment): this use case holds the port.Transactor and wraps only the write.
// The refund is idempotent at the provider on the payment id, so a retry, a redelivered message or a
// crash between the provider's answer and our commit refunds nothing twice.
//
// A provider that does not answer returns port.ErrProviderUnavailable WITHOUT the provider's words:
// the consumer retries it with backoff, and after the last attempt the message parks on
// payment.poison (alerting, eventbus poison metrics) while the payment stays REFUNDING.
type ProcessRefund struct {
	payments payment.Repository
	provider port.PaymentProvider
	events   port.EventPublisher
	tx       port.Transactor
	clock    port.Clock
}

func NewProcessRefund(payments payment.Repository, provider port.PaymentProvider, events port.EventPublisher, tx port.Transactor, clock port.Clock) *ProcessRefund {
	return &ProcessRefund{payments: payments, provider: provider, events: events, tx: tx, clock: clock}
}

func (uc *ProcessRefund) Execute(c context.Context, in dto.RefundInput) (*dto.Ack, error) {
	ctx := contextx.FromContext(c)
	if in.PaymentID == "" {
		return nil, payment.ErrInvalidID
	}

	p, err := uc.payments.FindByID(ctx, payment.ID(in.PaymentID))
	if err != nil {
		return nil, err
	}
	switch p.Status() {
	case payment.StatusRefunded:
		return &dto.Ack{}, nil // done by an earlier delivery
	case payment.StatusRefunding:
	default:
		return nil, payment.ErrNotRefunding
	}

	if err = uc.provider.Refund(ctx, dto.ProviderRefundInput{
		IdempotencyKey: p.ID().String(), Ref: p.ProviderRef(), Amount: mapper.ToMoneyOutput(p.Amount()),
	}); err != nil {
		return nil, port.ErrProviderUnavailable // fixed: the provider's own text is untrusted (#347)
	}

	err = uc.tx.Do(ctx, func(c context.Context) error {
		now := uc.clock.Now()
		eff, err := p.RefundSucceeded(now)
		if err != nil {
			return err
		}
		if err = uc.payments.Save(c, p); err != nil {
			return fmt.Errorf("save payment %s: %w", p.ID(), err)
		}

		return uc.events.Publish(c, p, eff.Publish(), now)
	})
	if errors.Is(err, payment.ErrStale) {
		return nil, err // someone else moved it: reload and look again on the redelivery
	}
	if err != nil {
		return nil, err
	}

	return &dto.Ack{}, nil
}
