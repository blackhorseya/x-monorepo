package payment

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

// MockControl is `payment-rpc mock pay|notify|fail` (docs/payment.md §9 S6): the customer and the gateway of the
// mock provider, played from outside the three payment-rpc processes. It works on the same payment.mock_* tables
// they do, so what it pays or breaks is what they see.
//
// It exists only for provider "mock" with payment.mock_control on, a database to share and the webhook secret
// the other processes sign with (an empty one would give this process a random secret nobody can verify). It
// opens no route and makes no call to the payment service: delivering a webhook is the caller's job.
type MockControl struct {
	provider *mock.Provider
	store    mock.Store
	payments payment.Repository
}

// NewMockControl refuses, naming the setting, unless the control entry may exist.
func NewMockControl(cfg config.Payment, db *gorm.DB) (*MockControl, error) {
	if cfg.Provider != mock.Name {
		return nil, fmt.Errorf("payment.provider is %q: the mock control only exists for the %q provider", cfg.Provider, mock.Name)
	}
	if !cfg.MockControl {
		return nil, errors.New("payment.mock_control (APP_PAYMENT_MOCK_CONTROL) is off: the mock control is for kind and tests, switch it on there")
	}
	if strings.TrimSpace(cfg.MockWebhookSecret) == "" {
		return nil, errors.New("payment.mock_webhook_secret (APP_PAYMENT_MOCK_WEBHOOK_SECRET) is empty: the control would sign with a secret no payment-rpc process can verify")
	}
	if db == nil {
		return nil, errors.New("postgres.url is empty: the mock control works on the mock tables the payment-rpc processes share")
	}

	p, err := ProvidePaymentProvider(cfg, db)
	if err != nil {
		return nil, err
	}

	return &MockControl{provider: p.(*mock.Provider), store: postgres.NewMockStore(db), payments: postgres.NewPaymentRepository(db)}, nil
}

// Payment is the delivery of a payment the customer made, and the checkout it was made at.
type Payment struct {
	Ref      string
	Delivery mock.Delivery
}

// Pay plays the customer paying the checkout of order, for the payment's own amount unless amount is given
// (a mismatch is a case worth rehearsing, §3 T4), and returns the signed "payment.captured" webhook. It can be
// delivered any number of times.
func (x *MockControl) Pay(c context.Context, order string, amount *kernel.Money) (Payment, error) {
	if strings.TrimSpace(order) == "" {
		return Payment{}, errors.New("--order is required: the order whose checkout is paid")
	}
	p, err := x.payments.FindByOrderID(c, order)
	if errors.Is(err, payment.ErrNotFound) {
		return Payment{}, fmt.Errorf("no payment for order %q", order)
	}
	if err != nil {
		return Payment{}, fmt.Errorf("find the payment of order %q: %w", order, err)
	}
	if p.ProviderRef() == "" {
		return Payment{}, fmt.Errorf("order %q has a payment but no checkout: StartPayment was never called, there is nothing to pay at", order)
	}
	paid := p.Amount()
	if amount != nil {
		paid = *amount
	}
	d, err := x.provider.Pay(p.ProviderRef(), paid)
	if err != nil {
		return Payment{}, err
	}

	return Payment{Ref: p.ProviderRef(), Delivery: d}, nil
}

// Notify makes a signed webhook of any type and id for a checkout without moving it: a redelivery (same id, same
// bytes), a notification of another type, or one to deliver before or after the others. The amount defaults to
// the checkout's.
func (x *MockControl) Notify(c context.Context, ref, id, typ string, amount *kernel.Money) (Payment, error) {
	if strings.TrimSpace(ref) == "" || strings.TrimSpace(id) == "" || strings.TrimSpace(typ) == "" {
		return Payment{}, errors.New("--ref, --id and --type are required")
	}
	var a kernel.Money
	if amount != nil {
		a = *amount
	} else {
		co, ok, err := x.store.Get(c, ref)
		if err != nil {
			return Payment{}, fmt.Errorf("read checkout %q: %w", ref, err)
		}
		if !ok {
			return Payment{}, fmt.Errorf("no checkout %q (give --amount and --currency to notify about one the mock does not know)", ref)
		}
		if a, err = kernel.NewMoney(co.Amount.Amount, co.Amount.Currency); err != nil {
			return Payment{}, fmt.Errorf("checkout %q has no usable amount: %w", ref, err)
		}
	}

	return Payment{Ref: ref, Delivery: x.provider.Notify(id, typ, ref, a)}, nil
}

// Fail makes the provider's operations fail (on) or work again (off), for every payment-rpc process that has
// payment.mock_control on. No ops = all of them.
func (x *MockControl) Fail(c context.Context, ops []string, on bool) error {
	if len(ops) == 0 {
		ops = mock.AllOps
	}
	for _, op := range ops {
		known := false
		for _, k := range mock.AllOps {
			known = known || k == op
		}
		if !known {
			return fmt.Errorf("unknown operation %q (want one of %s)", op, strings.Join(mock.AllOps, ", "))
		}
	}
	for _, op := range ops {
		if err := x.store.SetFault(c, op, on); err != nil {
			return err
		}
	}

	return nil
}
