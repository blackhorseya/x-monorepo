// Package memory holds the in-memory repository for local dev and tests. SPEC STAGE: it stores nothing.
package memory

import (
	"context"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
)

var _ payment.Repository = (*PaymentRepository)(nil)

type PaymentRepository struct{}

func NewPaymentRepository() *PaymentRepository { return &PaymentRepository{} }

func (i *PaymentRepository) FindByOrderID(context.Context, string) (*payment.Payment, error) {
	return nil, payment.ErrNotFound
}

func (i *PaymentRepository) Save(context.Context, *payment.Payment) error { return nil }
