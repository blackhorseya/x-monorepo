package usecase

import (
	"context"
	"errors"
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// ProcessWebhook applies a stored provider notification to its payment (docs/payment.md T3 / T4 / T8 /
// T9 and the D, D' and E columns of the §3 table), driven by the consumer of payment.v1.WebhookReceived
// inside one transaction (Transactional + Idempotent): the payment's new state, the events it raises
// (PaymentCaptured, PaymentRefundRequested) and the notification's processed mark commit together.
//
//   - payment.captured for a checkout some payment opened: Payment.Capture decides (amount and currency
//     must match; late money after expiry or cancellation is accepted and, if the order had ended,
//     goes back);
//   - payment.captured for a checkout NO payment opened: an orphan, alerted, marked processed;
//   - any other type: stored, ignored (marked processed).
//
// A notification already processed is left alone, so a redelivered message cannot capture twice.
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

func (uc *ProcessWebhook) Execute(c context.Context, in dto.WebhookReceivedInput) (*dto.Ack, error) {
	ctx := contextx.FromContext(c)

	w, err := uc.webhooks.Find(ctx, in.Provider, in.EventID)
	if err != nil {
		return nil, err
	}
	if w.Processed() {
		return &dto.Ack{}, nil
	}
	now := uc.clock.Now()

	if w.IsCapture() {
		p, err := uc.payments.FindByProviderRef(ctx, w.Provider(), w.Ref())
		switch {
		case errors.Is(err, payment.ErrNotFound):
			uc.alerts.Raise(ctx, payment.AlertOrphanWebhook, "")
		case err != nil:
			return nil, fmt.Errorf("find payment by provider reference: %w", err)
		default:
			eff := p.Capture(payment.SourceWebhook, w.Amount(), now)
			if err = uc.payments.Save(ctx, p); err != nil {
				return nil, fmt.Errorf("save payment of order %s: %w", p.OrderID(), err)
			}
			if err = uc.events.Publish(ctx, p, eff.Publish(), now); err != nil {
				return nil, err
			}
			if a := eff.Alert(); a != payment.AlertNone {
				uc.alerts.Raise(ctx, a, p.OrderID())
			}
		}
	}

	if err = uc.webhooks.MarkProcessed(ctx, w.Provider(), w.EventID(), now); err != nil {
		return nil, fmt.Errorf("mark webhook event processed: %w", err)
	}

	return &dto.Ack{}, nil
}
