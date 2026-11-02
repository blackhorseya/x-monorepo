// Package storetest is the one contract every mock.Store keeps (docs/payment.md §3「已知限制」): the
// in-memory store and the Postgres store run the same assertions, so "memory for task run, Postgres
// for a deployment" is one behaviour, not two.
package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
)

// New returns a store that is empty for this test (a fresh one, or one over truncated tables).
type New func(t *testing.T) mock.Store

func twd(n int64) dto.MoneyOutput { return dto.MoneyOutput{Amount: n, Currency: "TWD"} }

// Run runs the contract against stores from newStore.
func Run(t *testing.T, newStore New) {
	t.Helper()
	c := context.Background()
	ends := time.Date(2026, 11, 2, 1, 0, 0, 0, time.UTC)

	t.Run("a checkout is remembered with what it was opened for", func(t *testing.T) {
		s := newStore(t)
		if err := s.Create(c, "ref-1", mock.Checkout{PaymentID: "pay-1", Amount: twd(1000), URL: "https://mock-pay.invalid/checkout/ref-1", ExpiresAt: ends}); err != nil {
			t.Fatal(err)
		}
		got, ok, err := s.Get(c, "ref-1")
		if err != nil || !ok {
			t.Fatalf("get: %+v ok=%v err=%v", got, ok, err)
		}
		if got.PaymentID != "pay-1" || got.Amount != twd(1000) || got.URL != "https://mock-pay.invalid/checkout/ref-1" ||
			!got.ExpiresAt.Equal(ends) || got.Paid || got.Cancelled {
			t.Fatalf("checkout = %+v", got)
		}
		if _, ok, err = s.Get(c, "nope"); err != nil || ok {
			t.Fatalf("an unknown ref: found=%v err=%v", ok, err)
		}
	})

	t.Run("paying records what was paid, and paying again replaces it", func(t *testing.T) {
		s := newStore(t)
		_ = s.Create(c, "ref-1", mock.Checkout{PaymentID: "pay-1", Amount: twd(1000), URL: "u", ExpiresAt: ends})
		if ok, err := s.MarkPaid(c, "ref-1", twd(999)); err != nil || !ok {
			t.Fatalf("pay: ok=%v err=%v", ok, err)
		}
		if got, _, _ := s.Get(c, "ref-1"); !got.Paid || got.PaidWith != twd(999) {
			t.Fatalf("after paying 999: %+v", got)
		}
		if ok, err := s.MarkPaid(c, "ref-1", twd(1000)); err != nil || !ok {
			t.Fatalf("pay again: ok=%v err=%v", ok, err)
		}
		if got, _, _ := s.Get(c, "ref-1"); !got.Paid || got.PaidWith != twd(1000) {
			t.Fatalf("after paying again: %+v", got)
		}
		if ok, err := s.MarkPaid(c, "nope", twd(1)); err != nil || ok {
			t.Fatalf("an unknown ref: ok=%v err=%v", ok, err)
		}
	})

	t.Run("cancelling is recorded; an unknown ref is not found", func(t *testing.T) {
		s := newStore(t)
		_ = s.Create(c, "ref-1", mock.Checkout{PaymentID: "pay-1", Amount: twd(1), URL: "u", ExpiresAt: ends})
		if ok, err := s.MarkCancelled(c, "ref-1"); err != nil || !ok {
			t.Fatalf("cancel: ok=%v err=%v", ok, err)
		}
		if got, _, _ := s.Get(c, "ref-1"); !got.Cancelled || got.Paid {
			t.Fatalf("after cancelling: %+v", got)
		}
		if ok, err := s.MarkCancelled(c, "nope"); err != nil || ok {
			t.Fatalf("an unknown ref: ok=%v err=%v", ok, err)
		}
	})

	t.Run("a refund needs a paid checkout, and honours a key once", func(t *testing.T) {
		s := newStore(t)
		_ = s.Create(c, "ref-1", mock.Checkout{PaymentID: "pay-1", Amount: twd(1000), URL: "u", ExpiresAt: ends})
		if ok, err := s.Refund(c, "ref-1", "k1"); err != nil || ok {
			t.Fatalf("refund before anyone paid: ok=%v err=%v (nothing to give back)", ok, err)
		}
		if n, _ := s.Refunds(c, "ref-1"); n != 0 {
			t.Fatalf("a refused refund left %d rows", n)
		}
		if ok, err := s.Refund(c, "nope", "k1"); err != nil || ok {
			t.Fatalf("refund of an unknown ref: ok=%v err=%v", ok, err)
		}
		_, _ = s.MarkPaid(c, "ref-1", twd(1000))
		for range 3 {
			if ok, err := s.Refund(c, "ref-1", "k1"); err != nil || !ok {
				t.Fatalf("refund with k1: ok=%v err=%v", ok, err)
			}
		}
		if n, _ := s.Refunds(c, "ref-1"); n != 1 {
			t.Fatalf("the same key three times = %d refunds, want 1", n)
		}
		if ok, err := s.Refund(c, "ref-1", "k2"); err != nil || !ok {
			t.Fatalf("refund with k2: ok=%v err=%v", ok, err)
		}
		if n, _ := s.Refunds(c, "ref-1"); n != 2 {
			t.Fatalf("two keys = %d refunds, want 2", n)
		}
		if n, err := s.Refunds(c, "nope"); err != nil || n != 0 {
			t.Fatalf("refunds of an unknown ref: %d, %v", n, err)
		}
	})

	t.Run("every ref ever opened is listed, oldest first", func(t *testing.T) {
		s := newStore(t)
		for _, ref := range []string{"c-3", "a-1", "b-2"} {
			if err := s.Create(c, ref, mock.Checkout{PaymentID: "p", Amount: twd(1), URL: "u", ExpiresAt: ends}); err != nil {
				t.Fatal(err)
			}
		}
		refs, err := s.Refs(c)
		if err != nil || len(refs) != 3 || refs[0] != "c-3" || refs[1] != "a-1" || refs[2] != "b-2" {
			t.Fatalf("refs = %v, %v (want opening order c-3, a-1, b-2)", refs, err)
		}
	})

	// The same behaviour through a Provider on top of the store, including the fixed error a caller sees.
	t.Run("a provider over the store walks open, pay, refund, cancel", func(t *testing.T) {
		s := newStore(t)
		p := mock.New(mock.Options{Store: s, CheckoutTTL: 5 * time.Minute})
		co, err := p.CreateCheckout(c, dto.ProviderCheckoutInput{PaymentID: "pay-1", OrderID: "ord-1", Amount: twd(1000)})
		if err != nil {
			t.Fatal(err)
		}
		if got, err := p.GetPayment(c, co.Ref); err != nil || got.Paid {
			t.Fatalf("before paying: %+v, %v", got, err)
		}
		if err = p.Refund(c, dto.ProviderRefundInput{IdempotencyKey: "pay-1", Ref: co.Ref, Amount: twd(1000)}); !errors.Is(err, port.ErrProviderUnavailable) {
			t.Fatalf("a refund before payment: %v, want %v", err, port.ErrProviderUnavailable)
		}
		money, _ := kernel.NewMoney(999, "TWD")
		if _, err = p.Pay(co.Ref, money); err != nil {
			t.Fatal(err)
		}
		if got, err := p.GetPayment(c, co.Ref); err != nil || !got.Paid || got.Amount != twd(999) {
			t.Fatalf("after paying 999: %+v, %v", got, err)
		}
		for range 2 {
			if err = p.Refund(c, dto.ProviderRefundInput{IdempotencyKey: "pay-1", Ref: co.Ref, Amount: twd(999)}); err != nil {
				t.Fatal(err)
			}
		}
		if p.Refunds(co.Ref) != 1 || p.Opened() != 1 {
			t.Fatalf("refunds %d opened %d, want 1 and 1", p.Refunds(co.Ref), p.Opened())
		}
		if err = p.CancelCheckout(c, co.Ref); err != nil || !p.Cancelled(co.Ref) {
			t.Fatalf("cancel: %v, cancelled=%v", err, p.Cancelled(co.Ref))
		}
		if _, err = p.Pay("nope", money); err == nil {
			t.Fatal("paying an unknown checkout must fail")
		}
		if _, err = p.GetPayment(c, "nope"); !errors.Is(err, port.ErrProviderUnavailable) {
			t.Fatalf("asking about an unknown ref: %v, want %v", err, port.ErrProviderUnavailable)
		}
	})
}

// RunFaults is what docs/payment.md §9「失敗來源」relies on, kept on whatever store the mock sits on:
// an injected fault in any of the four provider calls reaches the caller as the fixed
// port.ErrProviderUnavailable, and the call never reaches the store (a faulty refund records no
// refund). Each case asserts the fault function was asked (positive control) and that lifting the
// fault lets the very same call through.
func RunFaults(t *testing.T, newStore New) {
	t.Helper()
	c := context.Background()

	type step struct {
		op   string
		call func(p *mock.Provider, ref string) error
		// recorded says whether the call, once let through, left a trace in the store
		recorded func(p *mock.Provider, ref string) bool
	}
	steps := []step{
		{mock.OpCreateCheckout, func(p *mock.Provider, _ string) error {
			_, err := p.CreateCheckout(c, dto.ProviderCheckoutInput{PaymentID: "pay-2", OrderID: "ord-2", Amount: twd(500)})

			return err
		}, func(p *mock.Provider, _ string) bool { return p.Opened() == 2 }},
		{mock.OpGetPayment, func(p *mock.Provider, ref string) error { _, err := p.GetPayment(c, ref); return err }, nil},
		{mock.OpCancelCheckout, func(p *mock.Provider, ref string) error { return p.CancelCheckout(c, ref) }, func(p *mock.Provider, ref string) bool { return p.Cancelled(ref) }},
		{mock.OpRefund, func(p *mock.Provider, ref string) error {
			return p.Refund(c, dto.ProviderRefundInput{IdempotencyKey: "k", Ref: ref, Amount: twd(1000)})
		}, func(p *mock.Provider, ref string) bool { return p.Refunds(ref) == 1 }},
	}
	for _, st := range steps {
		t.Run(st.op, func(t *testing.T) {
			faulty := ""
			asked := map[string]int{}
			p := mock.New(mock.Options{Store: newStore(t), Fault: func(op string) error {
				asked[op]++
				if op == faulty {
					return errors.New("secret provider text: card 4242 4242 4242 4242")
				}

				return nil
			}})
			co, err := p.CreateCheckout(c, dto.ProviderCheckoutInput{PaymentID: "pay-1", OrderID: "ord-1", Amount: twd(1000)})
			if err != nil {
				t.Fatal(err)
			}
			money, _ := kernel.NewMoney(1000, "TWD")
			if _, err = p.Pay(co.Ref, money); err != nil {
				t.Fatal(err)
			}

			faulty = st.op
			before := asked[st.op]
			err = st.call(p, co.Ref)
			if !errors.Is(err, port.ErrProviderUnavailable) || err.Error() != port.ErrProviderUnavailable.Error() {
				t.Fatalf("%s with a fault: %v, want exactly %v (the provider's own text must not get out)", st.op, err, port.ErrProviderUnavailable)
			}
			if asked[st.op] != before+1 {
				t.Fatalf("the fault function was asked %d times for %s, want %d: the fault never bit", asked[st.op]-before, st.op, 1)
			}
			if st.recorded != nil && st.recorded(p, co.Ref) {
				t.Fatalf("%s failed by injection and still left a trace in the store", st.op)
			}

			faulty = ""
			if err = st.call(p, co.Ref); err != nil {
				t.Fatalf("%s once the fault is lifted: %v", st.op, err)
			}
			if st.recorded != nil && !st.recorded(p, co.Ref) {
				t.Fatalf("%s succeeded without leaving its trace in the store", st.op)
			}
		})
	}
}

// RunStoredFaults is the contract of the faults a store keeps for `payment-rpc mock fail`: set per operation,
// seen by every provider over the store that asks for them, and ignored by one that does not. The in-memory
// store and the Postgres store (whose table accepts exactly mock.AllOps) both run it.
func RunStoredFaults(t *testing.T, newStore New) {
	t.Helper()
	c := context.Background()

	t.Run("a fault is per operation and stays until cleared", func(t *testing.T) {
		s := newStore(t)
		for _, op := range mock.AllOps {
			if on, err := s.Faulted(c, op); err != nil || on {
				t.Fatalf("%s faulted=%v err=%v on a fresh store", op, on, err)
			}
		}
		for _, op := range mock.AllOps { // every operation the mock names can be set: the Postgres table must accept them all
			if err := s.SetFault(c, op, true); err != nil {
				t.Fatalf("set %s: %v", op, err)
			}
			if err := s.SetFault(c, op, true); err != nil {
				t.Fatalf("setting %s again must succeed: %v", op, err)
			}
		}
		if err := s.SetFault(c, mock.OpRefund, false); err != nil {
			t.Fatal(err)
		}
		for _, op := range mock.AllOps {
			on, err := s.Faulted(c, op)
			if err != nil || on != (op != mock.OpRefund) {
				t.Fatalf("%s faulted=%v err=%v, want %v", op, on, err, op != mock.OpRefund)
			}
		}
		if err := s.SetFault(c, mock.OpRefund, false); err != nil {
			t.Fatalf("clearing what is not set must succeed: %v", err)
		}
	})

	t.Run("a provider asks the store only when told to", func(t *testing.T) {
		s := newStore(t)
		if err := s.SetFault(c, mock.OpCreateCheckout, true); err != nil {
			t.Fatal(err)
		}
		in := dto.ProviderCheckoutInput{PaymentID: "pay-1", OrderID: "ord-1", Amount: twd(1000)}

		if _, err := mock.New(mock.Options{Store: s}).CreateCheckout(c, in); err != nil {
			t.Fatalf("a provider that does not read faults failed on one: %v", err)
		}
		_, err := mock.New(mock.Options{Store: s, FaultsFromStore: true}).CreateCheckout(c, in)
		if !errors.Is(err, port.ErrProviderUnavailable) || err.Error() != port.ErrProviderUnavailable.Error() {
			t.Fatalf("a provider that reads faults answered a faulted operation with %v", err)
		}
		if err = s.SetFault(c, mock.OpCreateCheckout, false); err != nil {
			t.Fatal(err)
		}
		if _, err = mock.New(mock.Options{Store: s, FaultsFromStore: true}).CreateCheckout(c, in); err != nil {
			t.Fatalf("still failing after the fault was cleared: %v", err)
		}
	})
}
