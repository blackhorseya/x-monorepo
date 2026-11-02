package mock_test

import (
	"context"
	"errors"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock/storetest"
)

// The in-memory store keeps the contract the Postgres store keeps (infra/postgres runs the same one).
func TestMemoryStore_Contract(t *testing.T) {
	storetest.Run(t, func(*testing.T) mock.Store { return mock.NewMemoryStore() })
}

func TestMemoryStore_Faults(t *testing.T) {
	storetest.RunFaults(t, func(*testing.T) mock.Store { return mock.NewMemoryStore() })
}

// brokenStore cannot answer: its error text carries something that must never reach a caller.
type brokenStore struct{ mock.Store }

var errBroken = errors.New("pq: password=hunter2 connection refused")

func (brokenStore) Create(context.Context, string, mock.Checkout) error { return errBroken }
func (brokenStore) Get(context.Context, string) (mock.Checkout, bool, error) {
	return mock.Checkout{}, false, errBroken
}
func (brokenStore) MarkCancelled(context.Context, string) (bool, error)  { return false, errBroken }
func (brokenStore) Refund(context.Context, string, string) (bool, error) { return false, errBroken }

// A store that is down is "the provider is unavailable" to a caller, in the fixed words, never the
// store's own text (the same rule as an injected fault, #347).
func TestStoreDown_IsTheFixedUnavailableError(t *testing.T) {
	c := context.Background()
	p := mock.New(mock.Options{Store: brokenStore{mock.NewMemoryStore()}})
	checks := map[string]error{
		"create": func() error {
			_, err := p.CreateCheckout(c, dto.ProviderCheckoutInput{PaymentID: "p", OrderID: "o", Amount: dto.MoneyOutput{Amount: 1, Currency: "TWD"}})

			return err
		}(),
		"get":    func() error { _, err := p.GetPayment(c, "r"); return err }(),
		"cancel": p.CancelCheckout(c, "r"),
		"refund": p.Refund(c, dto.ProviderRefundInput{IdempotencyKey: "k", Ref: "r"}),
	}
	for name, err := range checks {
		if !errors.Is(err, port.ErrProviderUnavailable) || err.Error() != port.ErrProviderUnavailable.Error() {
			t.Fatalf("%s with the store down: %v, want exactly %v", name, err, port.ErrProviderUnavailable)
		}
	}
}
