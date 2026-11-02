package usecase

import (
	"context"
	"errors"
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/mapper"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/ownership"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// saveAttempts bounds the reload-and-retry when another writer changed the payment between the load
// and the save of one StartPayment.
const saveAttempts = 3

// StartPayment opens the provider's hosted checkout for an order's payment and returns the payment
// with its checkout URL (docs/payment.md T2).
//
// The provider is asked OUTSIDE any database transaction: a checkout is a call to another
// service, and no connection may be held while waiting for it. So this use case holds the
// port.Transactor itself and wraps only the write (the CreateOrder shape, §1 例外表), and the
// provider is never called inside it. A payment has ONE checkout, opened to end with its window: every
// later call returns the same URL without asking the provider. A second checkout could be paid as well,
// and nothing defines how a second payment is refunded; a notification of the first would also find no
// payment once its reference was replaced.
//
// The provider's own error is never passed on: it is untrusted text that may reflect a credential
// (#347), so every failure of the provider is the fixed port.ErrProviderUnavailable.
type StartPayment struct {
	payments payment.Repository
	provider port.PaymentProvider
	tx       port.Transactor
	clock    port.Clock
}

func NewStartPayment(payments payment.Repository, provider port.PaymentProvider, tx port.Transactor, clock port.Clock) *StartPayment {
	return &StartPayment{payments: payments, provider: provider, tx: tx, clock: clock}
}

func (uc *StartPayment) Execute(c context.Context, in dto.StartPaymentInput) (*dto.PaymentOutput, error) {
	ctx := contextx.FromContext(c)
	if in.OrderID == "" {
		return nil, payment.ErrInvalidOrderID
	}
	who := ownership.Caller{CustomerID: in.CustomerID, Admin: in.Admin}
	if err := who.Validate(); err != nil {
		return nil, err
	}

	p, err := uc.payments.FindByOrderID(ctx, in.OrderID)
	if err != nil {
		return nil, err
	}
	if err = uc.check(who, p); err != nil {
		return nil, err
	}
	if _, ok := p.OpenCheckout(); ok {
		return mapper.ToPaymentOutput(p), nil
	}

	co, err := uc.provider.CreateCheckout(ctx, dto.ProviderCheckoutInput{
		PaymentID: p.ID().String(), OrderID: p.OrderID(), Amount: mapper.ToMoneyOutput(p.Amount()),
		ExpiresAt: p.ExpiresAt(), // one checkout for the whole window: it ends with it
	})
	if err != nil {
		// Whatever it said, the caller and the log get the fixed sentinel only.
		return nil, port.ErrProviderUnavailable
	}

	// From here on a checkout exists at the provider. Unless it is the one saved on the payment it is
	// dropped on the way out, whatever the way out is (a refusal, a lost race, a failed save).
	kept := false
	defer func() {
		if !kept {
			uc.dropUnused(ctx, co)
		}
	}()

	for range saveAttempts {
		if err = p.AttachCheckout(uc.provider.Name(), co.Ref, co.URL, co.ExpiresAt, uc.clock.Now()); err != nil {
			return nil, err
		}
		err = uc.tx.Do(ctx, func(c context.Context) error { return uc.payments.Save(c, p) })
		if err == nil {
			kept = true

			return mapper.ToPaymentOutput(p), nil
		}
		if !errors.Is(err, payment.ErrStale) {
			return nil, fmt.Errorf("save payment of order %s: %w", in.OrderID, err)
		}

		// Another writer got in. Start again from what is stored: it may no longer be startable, or
		// the other writer may already have opened a checkout, which wins (one URL per payment).
		if p, err = uc.payments.FindByOrderID(ctx, in.OrderID); err != nil {
			return nil, err
		}
		if err = uc.check(who, p); err != nil {
			return nil, err
		}
		if _, ok := p.OpenCheckout(); ok {
			return mapper.ToPaymentOutput(p), nil
		}
	}

	return nil, fmt.Errorf("save payment of order %s: %w", in.OrderID, payment.ErrStale)
}

// check is everything that decides whether this caller may start this payment now.
func (uc *StartPayment) check(who ownership.Caller, p *payment.Payment) error {
	if err := ownership.CanPay(who, p); err != nil {
		return err
	}

	return p.CheckStart(uc.clock.Now())
}

// dropUnused cancels a checkout this call opened but did not keep. Best effort: the page expires on
// its own, so a failure here is not worth failing the request for.
func (uc *StartPayment) dropUnused(ctx context.Context, co dto.ProviderCheckoutOutput) {
	_ = uc.provider.CancelCheckout(ctx, co.Ref)
}
