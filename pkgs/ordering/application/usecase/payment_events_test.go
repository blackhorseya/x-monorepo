package usecase_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/ownership"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/memory"
)

// payEnv is the ordering use cases over one in-memory repository; ConfirmOrder is the only one that
// is given the require_payment setting, because CONFIRMED is the only place it is read.
type payEnv struct {
	repo     *memory.OrderRepository
	rec      *recorder
	create   *usecase.CreateOrder
	cancel   *usecase.CancelOrder
	complete *usecase.CompleteOrder
	captured *usecase.ApplyPaymentCaptured
	expired  *usecase.ApplyPaymentExpired
	get      *usecase.GetOrder
}

func newPayEnv() *payEnv {
	repo := memory.NewOrderRepository()
	clock := fixedClock{t: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
	rec := &recorder{}
	own := ownership.New("admin")

	return &payEnv{
		repo: repo, rec: rec,
		create:   usecase.NewCreateOrder(repo, memory.NewTransactor(), rec, fixedIDs{id: "o-1"}, clock, own, memory.NewFlatPriceList(500, "USD")),
		cancel:   usecase.NewCancelOrder(repo, rec, clock, own),
		complete: usecase.NewCompleteOrder(repo, rec, clock, own),
		captured: usecase.NewApplyPaymentCaptured(repo, rec, clock),
		expired:  usecase.NewApplyPaymentExpired(repo, rec, clock),
		get:      usecase.NewGetOrder(repo, own),
	}
}

// reach creates order o-1 and drives it to status. opts is what ConfirmOrder is built with: nil is the
// DEFAULT setting (no option at all), so the default cells test the default value itself.
func (e *payEnv) reach(t *testing.T, status string, opts ...usecase.ConfirmOption) {
	t.Helper()
	c := context.Background()
	clock := fixedClock{t: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
	if _, err := e.create.Execute(c, dto.CreateOrderInput{CustomerID: "c-1", Items: []dto.OrderLine{{SKU: "S", Quantity: 1}}}); err != nil {
		t.Fatal(err)
	}
	if status != "PENDING" {
		if _, err := usecase.NewConfirmOrder(e.repo, e.rec, clock, opts...).Execute(c, dto.ConfirmOrderInput{OrderID: "o-1"}); err != nil {
			t.Fatal(err)
		}
	}
	switch status {
	case "CANCELLED":
		if _, err := e.cancel.Execute(c, dto.CancelOrderInput{OrderID: "o-1", Source: dto.CancelSourceRPC}); err != nil {
			t.Fatal(err)
		}
	case "COMPLETED":
		if _, err := e.complete.Execute(c, dto.CompleteOrderInput{OrderID: "o-1"}); err != nil {
			t.Fatal(err)
		}
	}
	e.rec.events = nil // only what the payment event raises is asserted
}

func (e *payEnv) status(t *testing.T) string {
	t.Helper()
	out, err := e.get.Execute(context.Background(), dto.GetOrderInput{OrderID: "o-1"})
	if err != nil {
		t.Fatal(err)
	}

	return out.Status
}

func evNames(r *recorder) []string {
	out := []string{}
	for _, e := range r.events {
		out = append(out, fmt.Sprintf("%T", e))
	}

	return out
}

// docs/payment.md §8, ordering table, PaymentCaptured / PaymentExpired columns, for every cell the use
// cases can reach: snapshot × status. (PAID cells and a snapshot-true PENDING order are in the domain
// table.) The snapshot comes from ConfirmOrder; the "default" rows give it no option at all.
func TestPaymentEvents_ByCell(t *testing.T) {
	require := []usecase.ConfirmOption{usecase.WithRequirePayment(true)}
	type result struct {
		effect string
		err    error
		status string
		events []string
	}
	cases := []struct {
		name     string
		opts     []usecase.ConfirmOption
		reach    string
		captured result
		expired  result
	}{
		{"default/PENDING", nil, "PENDING", result{"", order.ErrNotConfirmed, "PENDING", []string{}}, result{"", order.ErrNotConfirmed, "PENDING", []string{}}},
		{"default/CONFIRMED", nil, "CONFIRMED", result{"unexpected", nil, "CONFIRMED", []string{}}, result{"ignored", nil, "CONFIRMED", []string{}}},
		{"default/CANCELLED", nil, "CANCELLED", result{"ignored", nil, "CANCELLED", []string{}}, result{"ignored", nil, "CANCELLED", []string{}}},
		{"default/COMPLETED", nil, "COMPLETED", result{"ignored", nil, "COMPLETED", []string{}}, result{"ignored", nil, "COMPLETED", []string{}}},
		{"require=true/CONFIRMED", require, "CONFIRMED", result{"applied", nil, "COMPLETED", []string{"order.Paid", "order.Completed"}}, result{"applied", nil, "CANCELLED", []string{"order.Cancelled"}}},
		{"require=true/CANCELLED", require, "CANCELLED", result{"", order.ErrCannotPay, "CANCELLED", []string{}}, result{"noop", nil, "CANCELLED", []string{}}},
	}
	for _, tc := range cases {
		for _, ev := range []string{"captured", "expired"} {
			t.Run(tc.name+"/"+ev, func(t *testing.T) {
				e := newPayEnv()
				e.reach(t, tc.reach, tc.opts...)
				var (
					out *dto.PaymentEventOutput
					err error
					w   result
				)
				if ev == "captured" {
					out, err = e.captured.Execute(context.Background(), dto.PaymentCapturedInput{OrderID: "o-1"})
					w = tc.captured
				} else {
					out, err = e.expired.Execute(context.Background(), dto.PaymentExpiredInput{OrderID: "o-1"})
					w = tc.expired
				}
				if !errors.Is(err, w.err) {
					t.Fatalf("err = %v, want %v", err, w.err)
				}
				if w.err == nil && out.Effect != w.effect {
					t.Errorf("effect = %q, want %q", out.Effect, w.effect)
				}
				if got := e.status(t); got != w.status {
					t.Errorf("stored status = %s, want %s", got, w.status)
				}
				if got := evNames(e.rec); fmt.Sprint(got) != fmt.Sprint(w.events) {
					t.Errorf("events = %v, want %v", got, w.events)
				}
			})
		}
	}
}

func TestPaymentEvents_UnknownOrderIsRefused(t *testing.T) {
	e := newPayEnv()
	if _, err := e.captured.Execute(context.Background(), dto.PaymentCapturedInput{OrderID: "nope"}); !errors.Is(err, order.ErrNotFound) {
		t.Errorf("captured err = %v, want order.not_found", err)
	}
	if _, err := e.expired.Execute(context.Background(), dto.PaymentExpiredInput{OrderID: "nope"}); !errors.Is(err, order.ErrNotFound) {
		t.Errorf("expired err = %v, want order.not_found", err)
	}
	if _, err := e.captured.Execute(context.Background(), dto.PaymentCapturedInput{}); !errors.Is(err, order.ErrInvalidID) {
		t.Errorf("captured with no order id: err = %v, want order.invalid_id", err)
	}
	if _, err := e.expired.Execute(context.Background(), dto.PaymentExpiredInput{}); !errors.Is(err, order.ErrInvalidID) {
		t.Errorf("expired with no order id: err = %v, want order.invalid_id", err)
	}
}

// Redelivery and ordering of the two events on one snapshot-true order (§4 亂序與重送, §8): whichever
// the order saw first decides; the other is refused or a no-op, and a CANCELLED order never comes back.
func TestPaymentEvents_RedeliveryAndOrder(t *testing.T) {
	c := context.Background()
	capIn, expIn := dto.PaymentCapturedInput{OrderID: "o-1"}, dto.PaymentExpiredInput{OrderID: "o-1"}
	require := usecase.WithRequirePayment(true)

	t.Run("captured twice: the second is a no-op that raises nothing", func(t *testing.T) {
		e := newPayEnv()
		e.reach(t, "CONFIRMED", require)
		if out, err := e.captured.Execute(c, capIn); err != nil || out.Effect != "applied" {
			t.Fatalf("first = %+v, %v", out, err)
		}
		e.rec.events = nil
		if out, err := e.captured.Execute(c, capIn); err != nil || out.Effect != "noop" {
			t.Fatalf("second = %+v, %v", out, err)
		}
		if len(e.rec.events) != 0 || e.status(t) != "COMPLETED" {
			t.Errorf("redelivery raised %v / status %s", evNames(e.rec), e.status(t))
		}
	})
	t.Run("expired twice", func(t *testing.T) {
		e := newPayEnv()
		e.reach(t, "CONFIRMED", require)
		if _, err := e.expired.Execute(c, expIn); err != nil {
			t.Fatal(err)
		}
		e.rec.events = nil
		if out, err := e.expired.Execute(c, expIn); err != nil || out.Effect != "noop" || len(e.rec.events) != 0 {
			t.Errorf("second = %+v, %v, events %v", out, err, evNames(e.rec))
		}
	})
	t.Run("captured, then expired: the order stays COMPLETED", func(t *testing.T) {
		e := newPayEnv()
		e.reach(t, "CONFIRMED", require)
		if _, err := e.captured.Execute(c, capIn); err != nil {
			t.Fatal(err)
		}
		if _, err := e.expired.Execute(c, expIn); !errors.Is(err, order.ErrCannotCancel) {
			t.Errorf("expired after captured: err = %v, want order.cannot_cancel", err)
		}
		if e.status(t) != "COMPLETED" {
			t.Errorf("status = %s", e.status(t))
		}
	})
	t.Run("expired, then captured: the order stays CANCELLED, never revived", func(t *testing.T) {
		e := newPayEnv()
		e.reach(t, "CONFIRMED", require)
		if _, err := e.expired.Execute(c, expIn); err != nil {
			t.Fatal(err)
		}
		e.rec.events = nil
		if _, err := e.captured.Execute(c, capIn); !errors.Is(err, order.ErrCannotPay) {
			t.Errorf("captured after expired: err = %v, want order.cannot_pay", err)
		}
		if e.status(t) != "CANCELLED" || len(e.rec.events) != 0 {
			t.Errorf("status %s, events %v", e.status(t), evNames(e.rec))
		}
	})
	t.Run("a customer cancel, then the captured event: refused, CANCELLED", func(t *testing.T) {
		e := newPayEnv()
		e.reach(t, "CONFIRMED", require)
		if _, err := e.cancel.Execute(c, dto.CancelOrderInput{OrderID: "o-1", Source: dto.CancelSourceRPC}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.captured.Execute(c, capIn); !errors.Is(err, order.ErrCannotPay) {
			t.Errorf("err = %v, want order.cannot_pay", err)
		}
		if e.status(t) != "CANCELLED" {
			t.Errorf("status = %s", e.status(t))
		}
	})
}

// The setting is read only where CONFIRMED happens. A snapshot-true order whose setting is later turned
// off — modelled by building a ConfirmOrder with the default and never using it again — gets the same
// answer to every payment event and every CompleteOrder / CancelOrder: nothing after ConfirmOrder takes
// the setting, so there is nothing to flip.
func TestPaymentEvents_TheSettingAfterConfirmIsNotConsulted(t *testing.T) {
	c := context.Background()
	for _, laterSetting := range []bool{true, false} {
		t.Run(fmt.Sprintf("setting now %v", laterSetting), func(t *testing.T) {
			e := newPayEnv()
			e.reach(t, "CONFIRMED", usecase.WithRequirePayment(true))
			// the process restarts with the setting flipped: a ConfirmOrder built with it exists, and is not used
			_ = usecase.NewConfirmOrder(e.repo, e.rec, fixedClock{}, usecase.WithRequirePayment(laterSetting))
			if _, err := e.complete.Execute(c, dto.CompleteOrderInput{OrderID: "o-1"}); !errors.Is(err, order.ErrPaymentRequired) {
				t.Fatalf("CompleteOrder err = %v, want order.payment_required", err)
			}
			if out, err := e.captured.Execute(c, dto.PaymentCapturedInput{OrderID: "o-1"}); err != nil || out.Effect != "applied" || e.status(t) != "COMPLETED" {
				t.Fatalf("captured = %+v, %v, status %s", out, err, e.status(t))
			}
		})
	}
}
