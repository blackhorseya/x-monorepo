package usecase

import (
	"context"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
)

// ReceiveWebhook is the webhook route's use case (docs/payment.md §7). SPEC STAGE: it accepts everything and stores nothing.
type ReceiveWebhook struct {
	webhooks payment.WebhookRepository
	provider port.PaymentProvider
	events   port.EventPublisher
	tx       port.Transactor
	clock    port.Clock
}

func NewReceiveWebhook(webhooks payment.WebhookRepository, provider port.PaymentProvider, events port.EventPublisher, tx port.Transactor, clock port.Clock) *ReceiveWebhook {
	return &ReceiveWebhook{webhooks: webhooks, provider: provider, events: events, tx: tx, clock: clock}
}

func (uc *ReceiveWebhook) Execute(context.Context, dto.WebhookInput) (*dto.Ack, error) {
	return &dto.Ack{}, nil
}
