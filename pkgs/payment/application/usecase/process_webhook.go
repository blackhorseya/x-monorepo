package usecase

import (
	"context"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
)

// ProcessWebhook applies a stored provider notification to its payment (docs/payment.md T3 / T4 / T8 / T9). SPEC STAGE: it does nothing.
type ProcessWebhook struct {
	payments payment.Repository
	webhooks payment.WebhookRepository
	events   port.EventPublisher
	alerts   port.Alerts
	clock    port.Clock
}

func NewProcessWebhook(payments payment.Repository, webhooks payment.WebhookRepository, events port.EventPublisher, alerts port.Alerts, clock port.Clock) *ProcessWebhook {
	return &ProcessWebhook{payments: payments, webhooks: webhooks, events: events, alerts: alerts, clock: clock}
}

func (uc *ProcessWebhook) Execute(context.Context, dto.WebhookReceivedInput) (*dto.Ack, error) {
	return &dto.Ack{}, nil
}
