// Package usecase holds the payment use cases: orchestration only, the rules live in
// domain/payment.
package usecase

import (
	"context"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/mapper"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/ownership"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// GetPayment reads the payment of an order, for its own customer or an admin.
type GetPayment struct {
	payments payment.Repository
}

func NewGetPayment(payments payment.Repository) *GetPayment { return &GetPayment{payments: payments} }

func (uc *GetPayment) Execute(c context.Context, in dto.GetPaymentInput) (*dto.PaymentOutput, error) {
	ctx := contextx.FromContext(c)
	if in.OrderID == "" {
		return nil, payment.ErrInvalidOrderID
	}
	who := ownership.Caller{CustomerID: in.CustomerID, Admin: in.Admin}
	if err := who.Validate(); err != nil {
		return nil, err
	}

	p, err := uc.payments.FindByOrderID(ctx, in.OrderID)
	if err != nil {
		return nil, err
	}
	if err := ownership.CanRead(who, p); err != nil {
		return nil, err
	}

	return mapper.ToPaymentOutput(p), nil
}
