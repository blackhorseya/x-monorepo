package usecase

import (
	"context"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
)

// ExpireSweep is `payment-rpc expire` (docs/payment.md T5 / T6 / T6' and the reconcile of T8 / T9).
// SPEC STAGE: a run does nothing.
type ExpireSweep struct {
	payments payment.Repository
	webhooks payment.WebhookRepository
	provider port.PaymentProvider
	events   port.EventPublisher
	alerts   port.Alerts
	tx       port.Transactor
	clock    port.Clock
	cfg      dto.SweepConfig
}

func NewExpireSweep(payments payment.Repository, webhooks payment.WebhookRepository, provider port.PaymentProvider, events port.EventPublisher, alerts port.Alerts, tx port.Transactor, clock port.Clock, cfg dto.SweepConfig) *ExpireSweep {
	return &ExpireSweep{payments: payments, webhooks: webhooks, provider: provider, events: events, alerts: alerts, tx: tx, clock: clock, cfg: cfg}
}

func (uc *ExpireSweep) Execute(context.Context, dto.Ack) (*dto.ExpireOutput, error) {
	return &dto.ExpireOutput{}, nil
}
