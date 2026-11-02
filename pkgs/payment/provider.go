// Package payment is the payment bounded context: the single payment of an order, collected through
// a provider's hosted checkout (docs/payment.md). Slice 2 holds the aggregate, the state machine,
// StartPayment / GetPayment and the mock provider; the saga (consume, webhook, expire, refund) is
// slice 3. ProviderSet is its only export to composition roots (apps/*).
package payment

import (
	"fmt"
	"log/slog"

	"github.com/google/wire"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/adapters/rpc"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/system"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
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

	// adapters
	rpc.NewPaymentService,
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
		p = mock.New(mock.Options{CheckoutTTL: cfg.MockCheckoutTTL})
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
