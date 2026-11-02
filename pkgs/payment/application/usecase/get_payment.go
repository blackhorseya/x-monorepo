// Package usecase holds the payment use cases: orchestration only, the rules live in
// domain/payment. SPEC STAGE: the use cases answer with an empty payment.
package usecase

import (
	"context"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
)

// GetPayment reads the payment of an order, for its own customer or an admin.
type GetPayment struct {
	payments payment.Repository
}

func NewGetPayment(payments payment.Repository) *GetPayment { return &GetPayment{payments: payments} }

func (uc *GetPayment) Execute(context.Context, dto.GetPaymentInput) (*dto.PaymentOutput, error) {
	return &dto.PaymentOutput{}, nil
}
