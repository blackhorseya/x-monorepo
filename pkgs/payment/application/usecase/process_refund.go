package usecase

import (
	"context"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
)

// ProcessRefund makes the refund of a REFUNDING payment (docs/payment.md T11). SPEC STAGE: it does nothing.
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

func (uc *ProcessRefund) Execute(context.Context, dto.RefundInput) (*dto.Ack, error) {
	return &dto.Ack{}, nil
}
