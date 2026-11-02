package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// stuckAfterSweeps is how many sweeps in a row may fail to ask the provider about a due payment before
// it is called stuck and alerted (docs/payment.md「連續失敗超過上限告警」).
const stuckAfterSweeps = 3

// ExpireSweep is `payment-rpc expire` (docs/payment.md T5 / T6 / T6' and the reconcile of T8 / T9),
// one run of the CronJob:
//
//  1. PENDING payments past their window: ASK THE PROVIDER FIRST. No money → EXPIRED (PaymentExpired);
//     money → CAPTURED or NEEDS_REVIEW exactly as a webhook would have (the webhook never came). A query
//     that fails moves NOTHING: "could not ask" is not "no money". A payment that never opened a checkout
//     cannot have been paid and is expired without a query.
//  2. EXPIRED / CANCELLED payments with a checkout, still inside the reconcile window: ask again, because
//     cancelling the checkout may have failed and the webhook may be missing; money found goes through the
//     same Capture (T8 / T9, and T10 when the order had ended). The window is bounded; the last query a
//     payment gets that fails is alerted.
//  3. retention of processed webhook notifications, bounded and last: its failure changes nothing above.
//
// Provider calls are outside any transaction (the fourth use case of the §1 exception table); each
// payment's write is its own small transaction, and a payment someone else changed meanwhile is skipped
// until the next run. NEEDS_REVIEW is never touched. Nothing here carries a word of the provider.
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

func (uc *ExpireSweep) Execute(c context.Context, _ dto.Ack) (*dto.ExpireOutput, error) {
	ctx := contextx.FromContext(c)
	out := &dto.ExpireOutput{}

	asked := map[string]bool{} // orders whose provider was already asked in this run
	if err := uc.expireDue(ctx, out, asked); err != nil {
		return out, err
	}
	if err := uc.reconcile(ctx, out, asked); err != nil {
		return out, err
	}
	uc.measure(ctx, out)
	uc.retention(ctx, out)
	if out.Failures > 0 {
		return out, fmt.Errorf("expire sweep: %d payments could not be saved", out.Failures)
	}

	return out, nil
}

func (uc *ExpireSweep) expireDue(ctx contextx.Contextx, out *dto.ExpireOutput, asked map[string]bool) error {
	now := uc.clock.Now()
	due, err := uc.payments.ListPendingDue(ctx, now, uc.cfg.Batch)
	if err != nil {
		return fmt.Errorf("list due payments: %w", err)
	}
	defer uc.markSwept(ctx, out, due)
	for _, p := range due {
		paid := false
		if p.ProviderRef() != "" {
			asked[p.OrderID()] = true
			got, err := uc.provider.GetPayment(ctx, p.ProviderRef())
			if err != nil {
				out.QueryFailures++
				if now.Sub(p.ExpiresAt()) >= stuckAfterSweeps*uc.cfg.Interval {
					uc.alerts.Raise(ctx, payment.AlertProviderQueryStuck, p.OrderID())
				}

				continue // could not ask: not "no money"
			}
			paid = got.Paid
			if paid {
				uc.apply(ctx, out, p, func(now time.Time) payment.Effect {
					return p.Capture(payment.SourceReconcile, toMoney(got.Amount), now)
				})

				continue
			}
		}
		uc.apply(ctx, out, p, func(now time.Time) payment.Effect { return p.Unpaid(now) })
	}

	return nil
}

func (uc *ExpireSweep) reconcile(ctx contextx.Contextx, out *dto.ExpireOutput, asked map[string]bool) error {
	now := uc.clock.Now()
	open, err := uc.payments.ListReconcilable(ctx, now.Add(-uc.cfg.ReconcileWindow), uc.cfg.Batch)
	if err != nil {
		return fmt.Errorf("list reconcilable payments: %w", err)
	}
	var queried []*payment.Payment
	defer func() { uc.markSwept(ctx, out, queried) }()
	for _, p := range open {
		if asked[p.OrderID()] || !p.Reconcilable(now, uc.cfg.ReconcileWindow) {
			continue // asked a moment ago, in the first pass of this very run; or out of its window
		}
		queried = append(queried, p)
		got, err := uc.provider.GetPayment(ctx, p.ProviderRef())
		if err != nil {
			out.QueryFailures++
			if !now.Add(uc.cfg.Interval).Before(p.ReconcileDeadline(uc.cfg.ReconcileWindow)) {
				uc.alerts.Raise(ctx, payment.AlertReconcileIncomplete, p.OrderID()) // the last chance failed
			}

			continue
		}
		if !got.Paid {
			continue
		}
		uc.apply(ctx, out, p, func(now time.Time) payment.Effect {
			return p.Capture(payment.SourceReconcile, toMoney(got.Amount), now)
		})
	}

	return nil
}

// markSwept moves the cursor past the payments this pass looked at, whatever the provider said (or
// failed to say): without it a payment that stays in its list — unpaid, or its query failing — keeps the
// front of a bounded list for ever and the ones behind it are never asked. A cursor that cannot be written
// is a failed run (it would repeat the same front).
func (uc *ExpireSweep) markSwept(ctx contextx.Contextx, out *dto.ExpireOutput, looked []*payment.Payment) {
	if len(looked) == 0 {
		return
	}
	ids := make([]payment.ID, len(looked))
	for n, p := range looked {
		ids[n] = p.ID()
	}
	if err := uc.payments.MarkSwept(ctx, ids, uc.clock.Now()); err != nil {
		out.Failures++
	}
}

// measure reports how far behind the reconcile is (docs/payment.md §3 補查): how many payments are owed
// a query and how long the least recently looked-at has waited. More than half the window is an alert:
// from there the runs left cannot be counted on to reach every payment, and the batch or the interval
// needs an operator.
func (uc *ExpireSweep) measure(ctx contextx.Contextx, out *dto.ExpireOutput) {
	now := uc.clock.Now()
	b, err := uc.payments.ReconcileBacklog(ctx, now.Add(-uc.cfg.ReconcileWindow))
	if err != nil {
		out.Failures++

		return
	}
	out.ReconcileEligible = b.Eligible()
	if !b.OldestUnchecked().IsZero() {
		out.OldestUnchecked = now.Sub(b.OldestUnchecked())
	}
	uc.alerts.Backlog(ctx, out.ReconcileEligible, out.OldestUnchecked)
	if out.OldestUnchecked > uc.cfg.ReconcileWindow/2 {
		uc.alerts.Raise(ctx, payment.AlertReconcileBacklog, "")
	}
}

// apply runs transition on p, then saves it and queues its events in one transaction. A lost optimistic
// lock is skipped (counted, it comes round again); any other write failure is counted and the run fails
// at its end: one payment's trouble must not stop the rest of the run, and must not pass unnoticed.
func (uc *ExpireSweep) apply(ctx contextx.Contextx, out *dto.ExpireOutput, p *payment.Payment, transition func(now time.Time) payment.Effect) {
	now := uc.clock.Now()
	before := p.Status()
	eff := transition(now)
	if p.Status() == before {
		return // nothing to write (e.g. a repeat that changes nothing)
	}
	err := uc.tx.Do(ctx, func(c context.Context) error {
		if err := uc.payments.Save(c, p); err != nil {
			return err
		}

		return uc.events.Publish(c, p, eff.Publish(), now)
	})
	if errors.Is(err, payment.ErrStale) {
		out.Conflicts++

		return
	}
	if err != nil {
		out.Failures++

		return
	}
	switch p.Status() {
	case payment.StatusExpired:
		out.Expired++
	case payment.StatusCaptured, payment.StatusRefunding:
		out.Captured++
	case payment.StatusNeedsReview:
		out.NeedsReview++
	}
	if a := eff.Alert(); a != payment.AlertNone {
		uc.alerts.Raise(ctx, a, p.OrderID())
	}
}

func (uc *ExpireSweep) retention(ctx contextx.Contextx, out *dto.ExpireOutput) {
	if uc.cfg.WebhookRetention <= 0 {
		return
	}
	n, err := uc.webhooks.DeleteProcessedBefore(ctx, uc.clock.Now().Add(-uc.cfg.WebhookRetention), uc.cfg.Batch)
	if err != nil {
		out.RetentionFailed = true

		return
	}
	out.RetentionDeleted = n
}

func toMoney(m dto.MoneyOutput) kernel.Money {
	money, err := kernel.NewMoney(m.Amount, m.Currency)
	if err != nil {
		return kernel.Money{} // never equal to a payment's amount: Capture files it as a mismatch
	}

	return money
}
