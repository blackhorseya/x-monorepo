package order_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
)

// names lists the pulled events by type, in order.
func names(evs []order.Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, fmt.Sprintf("%T", e))
	}

	return out
}

// docs/payment.md §8, ordering table, PaymentCaptured / PaymentExpired columns: every (snapshot, status)
// cell, decided by the order alone. Cells the use cases cannot reach (a snapshot-true order that
// is PENDING, a snapshot-false order that is PAID) are here too: "送進去要有確定結果".
func TestApplyPaymentEvents_EveryCell(t *testing.T) {
	type want struct {
		effect order.PaymentEffect
		err    error
		status order.Status
		events []string
	}
	const (
		paid      = "order.Paid"
		completed = "order.Completed"
		cancelled = "order.Cancelled"
	)
	cases := []struct {
		snapshot bool
		status   order.Status
		captured want
		expired  want
	}{
		// snapshot false: payment is not this order's business.
		{false, order.StatusPending, want{order.PaymentIgnored, order.ErrNotConfirmed, order.StatusPending, nil}, want{order.PaymentIgnored, order.ErrNotConfirmed, order.StatusPending, nil}},
		{false, order.StatusConfirmed, want{order.PaymentUnexpected, nil, order.StatusConfirmed, nil}, want{order.PaymentIgnored, nil, order.StatusConfirmed, nil}},
		{false, order.StatusPaid, want{order.PaymentIgnored, nil, order.StatusPaid, nil}, want{order.PaymentIgnored, nil, order.StatusPaid, nil}},
		{false, order.StatusCompleted, want{order.PaymentIgnored, nil, order.StatusCompleted, nil}, want{order.PaymentIgnored, nil, order.StatusCompleted, nil}},
		{false, order.StatusCancelled, want{order.PaymentIgnored, nil, order.StatusCancelled, nil}, want{order.PaymentIgnored, nil, order.StatusCancelled, nil}},
		// snapshot true.
		{true, order.StatusPending, want{order.PaymentIgnored, order.ErrNotConfirmed, order.StatusPending, nil}, want{order.PaymentIgnored, order.ErrNotConfirmed, order.StatusPending, nil}},
		{true, order.StatusConfirmed, want{order.PaymentApplied, nil, order.StatusCompleted, []string{paid, completed}}, want{order.PaymentApplied, nil, order.StatusCancelled, []string{cancelled}}},
		{true, order.StatusPaid, want{order.PaymentNoop, nil, order.StatusPaid, nil}, want{order.PaymentIgnored, order.ErrCannotCancel, order.StatusPaid, nil}},
		{true, order.StatusCompleted, want{order.PaymentNoop, nil, order.StatusCompleted, nil}, want{order.PaymentIgnored, order.ErrCannotCancel, order.StatusCompleted, nil}},
		{true, order.StatusCancelled, want{order.PaymentIgnored, order.ErrCannotPay, order.StatusCancelled, nil}, want{order.PaymentNoop, nil, order.StatusCancelled, nil}},
	}
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	for _, tc := range cases {
		for _, ev := range []struct {
			name  string
			apply func(*order.Order) (order.PaymentEffect, error)
			want  want
		}{
			{"captured", func(o *order.Order) (order.PaymentEffect, error) { return o.ApplyPaymentCaptured(now) }, tc.captured},
			{"expired", func(o *order.Order) (order.PaymentEffect, error) { return o.ApplyPaymentExpired(now) }, tc.expired},
		} {
			t.Run(fmt.Sprintf("snapshot=%v/%s/%s", tc.snapshot, tc.status, ev.name), func(t *testing.T) {
				o := orderIn(t, tc.status, tc.snapshot)
				effect, err := ev.apply(o)
				if !errors.Is(err, ev.want.err) {
					t.Fatalf("err = %v, want %v", err, ev.want.err)
				}
				if effect != ev.want.effect {
					t.Errorf("effect = %s, want %s", effect, ev.want.effect)
				}
				if o.Status() != ev.want.status {
					t.Errorf("status = %s, want %s", o.Status(), ev.want.status)
				}
				if got := names(o.PullEvents()); fmt.Sprint(got) != fmt.Sprint(ev.want.events) {
					t.Errorf("events = %v, want %v", got, ev.want.events)
				}
				if o.PaymentRequired() != tc.snapshot {
					t.Error("a payment event changed the snapshot")
				}
			})
		}
	}
}

func TestPaymentEffect_String(t *testing.T) {
	for e, want := range map[order.PaymentEffect]string{
		order.PaymentIgnored: "ignored", order.PaymentApplied: "applied", order.PaymentNoop: "noop",
		order.PaymentUnexpected: "unexpected", order.PaymentEffect(99): "unknown",
	} {
		if e.String() != want {
			t.Errorf("%d.String() = %q, want %q", e, e.String(), want)
		}
	}
}
