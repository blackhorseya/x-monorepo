package usecase

import (
	"context"
	"errors"
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// ReceiveWebhook is the webhook route's use case (docs/payment.md §7 Webhook): it checks the signature
// over the RAW body FIRST, then stores the notification and queues its processing in ONE transaction
// (payment.webhook_events + a WebhookReceived row in the outbox), so a crash between "stored" and
// "to be processed" cannot happen. A redelivery stores nothing and publishes nothing and is answered
// like the first one: the provider stops retrying.
//
// A signature that does not verify writes nothing at all and returns port.ErrInvalidSignature; a
// signed body that is not an event returns port.ErrMalformedWebhook. Neither carries a word of the
// request: the body and the signature are untrusted input and may hold anything.
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

func (uc *ReceiveWebhook) Execute(c context.Context, in dto.WebhookInput) (*dto.Ack, error) {
	ctx := contextx.FromContext(c)

	ev, err := uc.provider.VerifyWebhook(in.Body, in.Signature)
	switch {
	case errors.Is(err, port.ErrInvalidSignature):
		return nil, port.ErrInvalidSignature
	case err != nil:
		return nil, port.ErrMalformedWebhook
	}

	var amount kernel.Money
	if ev.Amount != (dto.MoneyOutput{}) {
		if amount, err = kernel.NewMoney(ev.Amount.Amount, ev.Amount.Currency); err != nil {
			return nil, port.ErrMalformedWebhook
		}
	}
	now := uc.clock.Now()
	w, err := payment.NewWebhookEvent(uc.provider.Name(), ev.ID, ev.Type, ev.Ref, amount, in.Body, now)
	if err != nil {
		return nil, port.ErrMalformedWebhook
	}

	err = uc.tx.Do(ctx, func(c context.Context) error {
		inserted, err := uc.webhooks.Insert(c, w)
		if err != nil {
			return err
		}
		if !inserted {
			return nil // a redelivery: already stored, already queued
		}

		return uc.events.PublishWebhookReceived(c, w.Provider(), w.EventID(), now)
	})
	if err != nil {
		return nil, fmt.Errorf("store webhook event: %w", err)
	}

	return &dto.Ack{}, nil
}
