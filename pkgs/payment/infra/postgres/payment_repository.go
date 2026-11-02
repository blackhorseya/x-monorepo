package postgres

import (
	"context"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
)

var _ payment.Repository = (*PaymentRepository)(nil)

// PaymentRepository persists Payment in payment.payments. SPEC STAGE: it stores nothing.
type PaymentRepository struct {
	db *gorm.DB
}

func NewPaymentRepository(db *gorm.DB) *PaymentRepository { return &PaymentRepository{db: db} }

func (i *PaymentRepository) FindByOrderID(context.Context, string) (*payment.Payment, error) {
	return nil, payment.ErrNotFound
}

func (i *PaymentRepository) Save(context.Context, *payment.Payment) error { return nil }
