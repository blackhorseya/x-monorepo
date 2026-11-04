package order_test

import (
	"errors"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
)

var allStatuses = []order.Status{order.StatusPending, order.StatusConfirmed, order.StatusPaid, order.StatusCompleted, order.StatusCancelled}

// The whole state machine, every (from, to) pair (docs/payment.md §2, §8): PAID is reached only
// from CONFIRMED, leaves only to COMPLETED, and never goes to CANCELLED; CANCELLED and COMPLETED
// are terminal, so a cancelled order cannot be revived by any event.
func TestStatus_CanTransitionTo_Table(t *testing.T) {
	allowed := map[[2]order.Status]bool{
		{order.StatusPending, order.StatusConfirmed}:   true,
		{order.StatusPending, order.StatusCancelled}:   true,
		{order.StatusConfirmed, order.StatusCompleted}: true,
		{order.StatusConfirmed, order.StatusCancelled}: true,
		{order.StatusConfirmed, order.StatusPaid}:      true,
		{order.StatusPaid, order.StatusCompleted}:      true,
	}
	for _, from := range allStatuses {
		for _, to := range allStatuses {
			if got, want := from.CanTransitionTo(to), allowed[[2]order.Status{from, to}]; got != want {
				t.Errorf("%s → %s: CanTransitionTo = %v, want %v", from, to, got, want)
			}
		}
	}
	if order.StatusUnknown.CanTransitionTo(order.StatusConfirmed) {
		t.Error("UNKNOWN → CONFIRMED must be refused")
	}
}

func orderIn(t *testing.T, status order.Status, paymentRequired bool) *order.Order {
	t.Helper()
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	o, err := order.Reconstitute(order.ReconstituteParams{
		ID: "o-pay", CustomerID: "c-1", Status: status, PaymentRequired: paymentRequired, CreatedAt: now, UpdatedAt: now, Version: 1,
		Items: []order.LineItem{mustItem(t, "A", 2, mustMoney(t, 250, "USD"))},
	})
	if err != nil {
		t.Fatal(err)
	}

	return o
}

// Confirm stores the setting it is given as the order's snapshot, and nothing changes it later.
func TestConfirm_TakesPaymentRequiredSnapshot(t *testing.T) {
	for _, on := range []bool{false, true} {
		o := orderIn(t, order.StatusPending, false)
		if err := o.Confirm(time.Now(), on); err != nil {
			t.Fatal(err)
		}
		if o.PaymentRequired() != on {
			t.Errorf("Confirm(requirePayment=%v): PaymentRequired = %v", on, o.PaymentRequired())
		}
		// A refused Confirm (already CONFIRMED) must not overwrite the snapshot.
		if err := o.Confirm(time.Now(), !on); !errors.Is(err, order.ErrCannotConfirm) {
			t.Fatalf("second Confirm err = %v", err)
		}
		if o.PaymentRequired() != on {
			t.Errorf("refused Confirm changed the snapshot to %v", o.PaymentRequired())
		}
	}
	// The snapshot is part of the persisted state.
	if !orderIn(t, order.StatusConfirmed, true).PaymentRequired() || orderIn(t, order.StatusConfirmed, false).PaymentRequired() {
		t.Error("Reconstitute does not carry PaymentRequired")
	}
	if orderIn(t, order.StatusPending, false).PaymentRequired() {
		t.Error("a PENDING order must not owe payment yet")
	}
}

// docs/payment.md §8, the ordering table's CompleteOrder / Cancel columns, for every (snapshot,
// status) cell: the answer depends on the snapshot and the status only.
func TestOrder_CompleteAndCancel_BySnapshotAndStatus(t *testing.T) {
	cells := []struct {
		status   order.Status
		required bool
		complete error // nil = allowed
		cancel   error
	}{
		{order.StatusPending, false, order.ErrCannotComplete, nil},
		{order.StatusConfirmed, false, nil, nil},
		{order.StatusConfirmed, true, order.ErrPaymentRequired, nil},
		{order.StatusPaid, true, nil, order.ErrCannotCancel}, // slice 1 cannot reach it through a use case; the domain still defines it
		{order.StatusPaid, false, nil, order.ErrCannotCancel},
		{order.StatusCompleted, false, order.ErrCannotComplete, order.ErrCannotCancel},
		{order.StatusCompleted, true, order.ErrCannotComplete, order.ErrCannotCancel},
		{order.StatusCancelled, false, order.ErrCannotComplete, order.ErrCannotCancel},
		{order.StatusCancelled, true, order.ErrCannotComplete, order.ErrCannotCancel},
	}
	for _, tc := range cells {
		name := tc.status.String() + "/required=" + map[bool]string{true: "true", false: "false"}[tc.required]
		t.Run(name, func(t *testing.T) {
			o := orderIn(t, tc.status, tc.required)
			if err := o.Complete(time.Now()); !errors.Is(err, tc.complete) {
				t.Errorf("Complete err = %v, want %v", err, tc.complete)
			}
			if tc.complete != nil && o.Status() != tc.status {
				t.Errorf("a refused Complete moved the order to %s", o.Status())
			}
			o = orderIn(t, tc.status, tc.required)
			if err := o.Cancel(time.Now(), order.CauseRPCRequest); !errors.Is(err, tc.cancel) {
				t.Errorf("Cancel err = %v, want %v", err, tc.cancel)
			}
		})
	}
}

// MarkPaid: CONFIRMED only; it raises Paid and leaves the snapshot alone.
func TestMarkPaid(t *testing.T) {
	for _, from := range allStatuses {
		o := orderIn(t, from, true)
		at := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
		err := o.MarkPaid(at)
		if from != order.StatusConfirmed {
			if !errors.Is(err, order.ErrCannotPay) || o.Status() != from || len(o.PullEvents()) != 0 {
				t.Errorf("MarkPaid from %s: err = %v, status = %s, want ErrCannotPay and no change", from, err, o.Status())
			}
			continue
		}
		if err != nil || o.Status() != order.StatusPaid || !o.PaymentRequired() || !o.UpdatedAt().Equal(at) {
			t.Fatalf("MarkPaid from CONFIRMED: err = %v, status = %s, required = %v", err, o.Status(), o.PaymentRequired())
		}
		evs := o.PullEvents()
		paid, ok := evs[0].(order.Paid)
		if len(evs) != 1 || !ok || paid.OrderID() != "o-pay" || paid.CustomerID() != "c-1" || !paid.OccurredAt().Equal(at) {
			t.Fatalf("MarkPaid events = %+v", evs)
		}
		// Paid money is not undone: no cancel, and a second MarkPaid is refused.
		if err := o.Cancel(at, order.CauseRPCRequest); !errors.Is(err, order.ErrCannotCancel) {
			t.Errorf("Cancel after MarkPaid err = %v", err)
		}
		if err := o.MarkPaid(at); !errors.Is(err, order.ErrCannotPay) {
			t.Errorf("second MarkPaid err = %v", err)
		}
		if err := o.Complete(at); err != nil || o.Status() != order.StatusCompleted {
			t.Errorf("Complete after MarkPaid: err = %v, status = %s", err, o.Status())
		}
	}
}

// Confirmed carries the order's total, taken at the moment of Confirm.
func TestConfirmed_CarriesTotal(t *testing.T) {
	o := orderIn(t, order.StatusPending, false)
	if err := o.Confirm(time.Now(), true); err != nil {
		t.Fatal(err)
	}
	confirmed := o.PullEvents()[0].(order.Confirmed)
	if confirmed.TotalAmount() != 500 || confirmed.TotalCurrency() != "USD" {
		t.Fatalf("Confirmed total = %d %s, want 500 USD", confirmed.TotalAmount(), confirmed.TotalCurrency())
	}
}
