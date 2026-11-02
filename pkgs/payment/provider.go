// Package payment is the payment bounded context: the single payment of an order, collected through
// a provider's hosted checkout (docs/payment.md). Slice 2 holds the aggregate, the state machine,
// StartPayment / GetPayment and the mock provider; the saga (consume, webhook, expire, refund) is
// slice 3. ProviderSet is its only export to composition roots (apps/*).
package payment

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/wire"
	"go.opentelemetry.io/otel/metric"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/adapters/consumer"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/adapters/events"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/adapters/rpc"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/adapters/webhook"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/alerts"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/system"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// Schema is the Postgres schema of this BC (tables, migrations).
const Schema = postgres.Schema

var ProviderSet = wire.NewSet(
	// infra → ports / repositories
	ProvidePaymentRepository,
	ProvideTransactor,
	system.NewClock,
	wire.Bind(new(port.Clock), new(*system.Clock)),

	// application (decorated; see decorate / decorateNoTx). *usecasex.Recorder is shared
	// across BCs: apps provide it.
	ProvideStartPayment,
	ProvideGetPayment,
	ProvideWebhookRepository,
	events.NewPublisher,
	wire.Bind(new(port.EventPublisher), new(*events.Publisher)),
	ProvideReceiveWebhook,

	// adapters
	rpc.NewPaymentService,
	webhook.NewHandler,
	WebhookRoute,
)

// ProvidePaymentRepository picks the persistence backend from the composition root's database
// handle: nil (APP_POSTGRES_URL unset) means in-memory.
func ProvidePaymentRepository(db *gorm.DB, logger *slog.Logger) payment.Repository {
	if db == nil {
		logger.Warn("payment: APP_POSTGRES_URL unset, using in-memory payment repository")
		return memory.NewPaymentRepository()
	}

	return postgres.NewPaymentRepository(db)
}

// ProvideTransactor pairs with the repository: a real transaction on Postgres, a pass-through in memory.
func ProvideTransactor(db *gorm.DB) port.Transactor {
	if db == nil {
		return memory.NewTransactor()
	}

	return postgres.NewTransactor(db)
}

// ProvidePaymentProvider is not in ProviderSet: the composition root builds the provider before it
// wires anything (so a configuration that cannot work refuses to start before a database is opened)
// and hands it to the injector, which is also how a test puts its own provider behind the real server.
//
// It builds the payment provider payment.provider names and refuses to start
// on a configuration that would lose money without a sound: reconcile_window must outlast the
// provider's checkout pages (docs/payment.md §3「補查有界」), or a customer paying at the very end
// of a page could pay after the sweep stopped looking.
func ProvidePaymentProvider(cfg config.Payment) (port.PaymentProvider, error) {
	var p port.PaymentProvider
	switch cfg.Provider {
	case mock.Name:
		p = mock.New(mock.Options{CheckoutTTL: cfg.MockCheckoutTTL, RedeliveryWindow: cfg.MockRedeliveryWindow, Secret: []byte(cfg.MockWebhookSecret)})
	default:
		return nil, fmt.Errorf("payment: payment.provider %q is not supported (the first version has %q only)", cfg.Provider, mock.Name)
	}

	if cfg.ReconcileWindow <= p.CheckoutValidity() {
		return nil, fmt.Errorf("payment: payment.reconcile_window (%s) must be longer than the provider's checkout validity (%s)", cfg.ReconcileWindow, p.CheckoutValidity())
	}

	return p, nil
}

// decorate is the one place that decides which cross-cutting decorators every read use case of
// this BC gets (outermost first).
func decorate[I, O any](name string, rec *usecasex.Recorder, uc usecasex.UseCase[I, O]) usecasex.UseCase[I, O] {
	return usecasex.Chain(uc,
		usecasex.Metrics[I, O](rec, name),
		usecasex.Logging[I, O](name),
	)
}

// decorateNoTx is for a use case that calls the provider (docs/payment.md §1 例外表): it opens its
// own transaction (port.Transactor) around just the write, because the provider call must not hold a
// database connection. Metrics and Logging still wrap the whole call. There is no Transactional here,
// and no inbox: the use case has no consumer path in this slice.
func decorateNoTx[I, O any](name string, rec *usecasex.Recorder, uc usecasex.UseCase[I, O]) usecasex.UseCase[I, O] {
	return usecasex.Chain(uc,
		usecasex.Metrics[I, O](rec, name),
		usecasex.Logging[I, O](name),
	)
}

func ProvideStartPayment(payments payment.Repository, provider port.PaymentProvider, tx port.Transactor, clock port.Clock, rec *usecasex.Recorder) rpc.StartPaymentUC {
	return decorateNoTx("payment.start_payment", rec, usecase.NewStartPayment(payments, provider, tx, clock))
}

func ProvideGetPayment(payments payment.Repository, rec *usecasex.Recorder) rpc.GetPaymentUC {
	return decorate("payment.get_payment", rec, usecase.NewGetPayment(payments))
}

// ConsumerProviderSet is what `payment-rpc consume` adds to ProviderSet: the outbox publisher, the
// consumer inbox, the outcome store, the alerts, the consumer use cases (decorated here) and the
// subscriptions the composition root registers on the router.
var ConsumerProviderSet = wire.NewSet(
	ProviderSet,
	ProvideOutcomeRepository,
	ProvideInbox,
	system.NewIDGenerator,
	wire.Bind(new(port.IDGenerator), new(*system.IDGenerator)),
	ProvideAlerts,

	ProvideRecordOrderConfirmed,
	ProvideRecordOrderCancelled,
	ProvideRecordOrderCompleted,
	ProvideProcessRefund,
	ProvideProcessWebhook,

	consumer.NewWebhookReceivedHandler,
	consumer.NewOrderConfirmedHandler,
	consumer.NewOrderCancelledHandler,
	consumer.NewOrderCompletedHandler,
	consumer.NewRefundHandler,
	ProvideSubscriptions,
)

// Subscriptions lists what `payment-rpc consume` handles; the composition root registers them on
// the router with payment's poison topic. A named type, so wire can tell it from any other BC's.
type Subscriptions []eventbus.Subscription

// ProvideSubscriptions binds each consumed topic to its handler.
func ProvideSubscriptions(
	confirmed *consumer.OrderConfirmedHandler, cancelled *consumer.OrderCancelledHandler,
	completed *consumer.OrderCompletedHandler, refund *consumer.RefundHandler, webhook *consumer.WebhookReceivedHandler,
) Subscriptions {
	return Subscriptions{
		{Topic: consumer.TopicOrderConfirmed, Handler: confirmed},
		{Topic: consumer.TopicOrderCancelled, Handler: cancelled},
		{Topic: consumer.TopicOrderCompleted, Handler: completed},
		{Topic: consumer.TopicPaymentRefundRequested, Handler: refund},
		{Topic: consumer.TopicWebhookReceived, Handler: webhook},
	}
}

// ProvideOutcomeRepository pairs with ProvidePaymentRepository.
func ProvideOutcomeRepository(db *gorm.DB) payment.OutcomeRepository {
	if db == nil {
		return memory.NewOutcomeRepository()
	}

	return postgres.NewOutcomeRepository(db)
}

// ProvideInbox is the consumer inbox: payment.inbox on Postgres, none in memory (a nil Inbox makes
// Idempotent a pass-through; the in-memory bus is at most once anyway).
func ProvideInbox(db *gorm.DB) port.Inbox {
	if db == nil {
		return nil
	}

	return platformpg.NewInbox(db, Schema)
}

func ProvideAlerts(logger *slog.Logger, meter metric.MeterProvider) (port.Alerts, error) {
	return alerts.New(logger, meter)
}

// decorateTx is decorate for use cases that write on the consumer path: one transaction around the
// use case, so its writes and outbox events commit or roll back together; inside it Idempotent
// records the consumed message id in payment.inbox (same transaction) and drops a redelivery before
// any aggregate is loaded.
func decorateTx[I, O any](name string, rec *usecasex.Recorder, tx port.Transactor, inbox port.Inbox, uc usecasex.UseCase[I, O]) usecasex.UseCase[I, O] {
	return usecasex.Chain(uc,
		usecasex.Metrics[I, O](rec, name),
		usecasex.Logging[I, O](name),
		usecasex.Transactional[I, O](tx),
		usecasex.Idempotent[I, O](inbox),
	)
}

func ProvideRecordOrderConfirmed(payments payment.Repository, outcomes payment.OutcomeRepository, ids port.IDGenerator, clock port.Clock, al port.Alerts, cfg config.Payment, tx port.Transactor, inbox port.Inbox, rec *usecasex.Recorder) consumer.OrderConfirmedUC {
	return decorateTx("payment.record_order_confirmed", rec, tx, inbox, usecase.NewRecordOrderConfirmed(payments, outcomes, ids, clock, al, cfg.Window))
}

func ProvideRecordOrderCancelled(payments payment.Repository, outcomes payment.OutcomeRepository, ev port.EventPublisher, clock port.Clock, provider port.PaymentProvider, logger *slog.Logger, tx port.Transactor, inbox port.Inbox, rec *usecasex.Recorder) consumer.OrderCancelledUC {
	return endedChain("payment.record_order_cancelled", rec, tx, inbox, provider, logger, usecase.NewRecordOrderCancelled(payments, outcomes, ev, clock))
}

func ProvideRecordOrderCompleted(payments payment.Repository, outcomes payment.OutcomeRepository, ev port.EventPublisher, clock port.Clock, provider port.PaymentProvider, logger *slog.Logger, tx port.Transactor, inbox port.Inbox, rec *usecasex.Recorder) consumer.OrderCompletedUC {
	return endedChain("payment.record_order_completed", rec, tx, inbox, provider, logger, usecase.NewRecordOrderCompleted(payments, outcomes, ev, clock))
}

// endedChain is decorateTx plus one decorator between the logging and the transaction: after the
// transaction committed, the provider's checkout is cancelled when the use case said so. The call is
// outside the transaction (a provider is never called holding a connection) and best effort: its
// failure is logged as a fixed category and changes nothing, because a checkout that stays open is
// found by the reconcile sweep (docs/payment.md T7 / T8 / T9).
func endedChain(name string, rec *usecasex.Recorder, tx port.Transactor, inbox port.Inbox, provider port.PaymentProvider, logger *slog.Logger, uc usecasex.UseCase[dto.OrderEndedInput, *dto.OrderEndedOutput]) usecasex.UseCase[dto.OrderEndedInput, *dto.OrderEndedOutput] {
	return usecasex.Chain(uc,
		usecasex.Metrics[dto.OrderEndedInput, *dto.OrderEndedOutput](rec, name),
		usecasex.Logging[dto.OrderEndedInput, *dto.OrderEndedOutput](name),
		cancelCheckoutAfterCommit(provider, logger),
		usecasex.Transactional[dto.OrderEndedInput, *dto.OrderEndedOutput](tx),
		usecasex.Idempotent[dto.OrderEndedInput, *dto.OrderEndedOutput](inbox),
	)
}

func cancelCheckoutAfterCommit(provider port.PaymentProvider, logger *slog.Logger) usecasex.Decorator[dto.OrderEndedInput, *dto.OrderEndedOutput] {
	return func(next usecasex.UseCase[dto.OrderEndedInput, *dto.OrderEndedOutput]) usecasex.UseCase[dto.OrderEndedInput, *dto.OrderEndedOutput] {
		return usecasex.Func[dto.OrderEndedInput, *dto.OrderEndedOutput](func(c context.Context, in dto.OrderEndedInput) (*dto.OrderEndedOutput, error) {
			ctx := contextx.FromContext(c)
			out, err := next.Execute(ctx, in)
			if err != nil || out == nil || out.CancelCheckoutRef == "" {
				return out, err
			}
			if cerr := provider.CancelCheckout(ctx, out.CancelCheckoutRef); cerr != nil {
				logger.WarnContext(ctx, "cancel checkout failed", "code", "payment.cancel_checkout_failed", "order_id", in.OrderID)
			}

			return out, nil
		})
	}
}

// ProvideProcessRefund drives the refund call: no Transactional (the provider is called outside any
// transaction; the use case wraps only its write) and no inbox (the provider call is idempotent on
// the payment id, and a payment already REFUNDED answers with nothing to do).
func ProvideProcessRefund(payments payment.Repository, provider port.PaymentProvider, ev port.EventPublisher, tx port.Transactor, clock port.Clock, rec *usecasex.Recorder) consumer.RefundUC {
	return decorateNoTx("payment.process_refund", rec, usecase.NewProcessRefund(payments, provider, ev, tx, clock))
}

// ProvideWebhookRepository pairs with ProvidePaymentRepository.
func ProvideWebhookRepository(db *gorm.DB) payment.WebhookRepository {
	if db == nil {
		return memory.NewWebhookRepository()
	}

	return postgres.NewWebhookRepository(db)
}

// ProvideReceiveWebhook is the webhook route's use case: it verifies, then writes in a transaction it
// opens itself, so it is not wrapped in Transactional (decorateNoTx). The provider's verification is
// local computation, but the shape is the same as the other provider-facing use cases: nothing about
// the request is held inside a transaction while it is judged.
func ProvideReceiveWebhook(webhooks payment.WebhookRepository, provider port.PaymentProvider, ev port.EventPublisher, tx port.Transactor, clock port.Clock, rec *usecasex.Recorder) webhook.ReceiveUC {
	return decorateNoTx("payment.receive_webhook", rec, usecase.NewReceiveWebhook(webhooks, provider, ev, tx, clock))
}

// WebhookRoute mounts the provider's webhook next to the probes (server.WithHTTPRoute): no API key, the
// signature is the credential.
func WebhookRoute(h *webhook.Handler) server.Option {
	return server.WithHTTPRoute(http.MethodPost, webhook.Path, h)
}

func ProvideProcessWebhook(payments payment.Repository, webhooks payment.WebhookRepository, ev port.EventPublisher, al port.Alerts, clock port.Clock, tx port.Transactor, inbox port.Inbox, rec *usecasex.Recorder) consumer.WebhookUC {
	return decorateTx("payment.process_webhook", rec, tx, inbox, usecase.NewProcessWebhook(payments, webhooks, ev, al, clock))
}
