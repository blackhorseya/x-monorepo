package usecase

import (
	"context"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
)

// StartPayment opens the provider's hosted checkout for an order's payment (docs/payment.md T2).
type StartPayment struct {
	payments payment.Repository
	provider port.PaymentProvider
	tx       port.Transactor
	clock    port.Clock
}

func NewStartPayment(payments payment.Repository, provider port.PaymentProvider, tx port.Transactor, clock port.Clock) *StartPayment {
	return &StartPayment{payments: payments, provider: provider, tx: tx, clock: clock}
}

func (uc *StartPayment) Execute(context.Context, dto.StartPaymentInput) (*dto.PaymentOutput, error) {
	return &dto.PaymentOutput{}, nil
}
