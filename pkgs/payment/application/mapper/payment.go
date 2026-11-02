// Package mapper turns domain objects into dto.
package mapper

import (
	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
)

func ToPaymentOutput(p *payment.Payment) *dto.PaymentOutput {
	return &dto.PaymentOutput{
		ID:                p.ID().String(),
		OrderID:           p.OrderID(),
		CustomerID:        p.CustomerID(),
		Amount:            ToMoneyOutput(p.Amount()),
		Status:            p.Status().String(),
		ExpiresAt:         p.ExpiresAt(),
		CheckoutURL:       p.CheckoutURL(),
		CheckoutExpiresAt: p.CheckoutExpiresAt(),
		UpdateTime:        p.UpdatedAt(),
	}
}

// ToMoneyOutput is an amount as the application boundary carries it.
func ToMoneyOutput(m kernel.Money) dto.MoneyOutput {
	return dto.MoneyOutput{Amount: m.Amount(), Currency: m.Currency()}
}
