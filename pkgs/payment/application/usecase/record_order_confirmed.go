package usecase

import (
	"context"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
)

// RecordOrderConfirmed creates the payment of a CONFIRMED order (docs/payment.md T1). SPEC STAGE: it does nothing.
type RecordOrderConfirmed struct {
	payments payment.Repository
	outcomes payment.OutcomeRepository
	ids      port.IDGenerator
	clock    port.Clock
	alerts   port.Alerts
	window   time.Duration
}

func NewRecordOrderConfirmed(payments payment.Repository, outcomes payment.OutcomeRepository, ids port.IDGenerator, clock port.Clock, alerts port.Alerts, window time.Duration) *RecordOrderConfirmed {
	return &RecordOrderConfirmed{payments: payments, outcomes: outcomes, ids: ids, clock: clock, alerts: alerts, window: window}
}

func (uc *RecordOrderConfirmed) Execute(context.Context, dto.OrderConfirmedInput) (*dto.Ack, error) {
	return &dto.Ack{}, nil
}
