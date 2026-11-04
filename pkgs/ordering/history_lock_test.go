package ordering_test

import (
	"context"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
)

// The lock migration 0011 holds on ordering.order_history is not confined to the history: every write
// that raises an order event UPDATEs ordering.orders and then INSERTs a history row in the same
// transaction (Save → history.Append), and GetOrderHistory reads the table. docs/operations.md lists
// exactly the use cases below; this test is the list.
//
// Session A holds ACCESS EXCLUSIVE on the table, the mode ALTER TABLE takes. Session B calls the real
// use case with a short deadline: it must wait and time out, and the transaction must roll back whole —
// the order unchanged and the outbox without a new row. The control is the same call succeeding once
// the lock is released, so the failure is the lock's and not the fixture's.
func TestOrderHistory_TableLockBlocksTheWritePath(t *testing.T) {
	db := historyDB(t)

	outboxRows := func(s *hstack) int64 {
		t.Helper()
		var n int64
		err := s.db.Raw(`SELECT (SELECT count(*) FROM ordering.events_v1_ordercreated) + (SELECT count(*) FROM ordering.events_v1_orderconfirmed) +
			(SELECT count(*) FROM ordering.events_v1_orderpaid) + (SELECT count(*) FROM ordering.events_v1_ordercompleted) +
			(SELECT count(*) FROM ordering.events_v1_ordercancelled)`).Scan(&n).Error
		if err != nil {
			t.Fatal(err)
		}

		return n
	}
	status := func(s *hstack, id string) string {
		t.Helper()
		var st string
		if err := s.db.Raw(`SELECT status FROM ordering.orders WHERE id = ?`, id).Scan(&st).Error; err != nil {
			t.Fatal(err)
		}

		return st
	}

	type op struct {
		name       string
		requirePay bool
		prepare    func(s *hstack, id string) // brings the order to the state the use case acts on
		run        func(s *hstack, ctx context.Context, id string) error
		wantBefore string // order status while the lock is held (and before the control)
		wantAfter  string // order status once the control has run
	}
	confirmed := func(s *hstack, id string) { s.newOrder(asCustomer("alice"), id, "alice"); s.doConfirm(id) }
	ops := []op{
		{"CreateOrder", false, func(s *hstack, id string) {},
			func(s *hstack, ctx context.Context, id string) error {
				s.ids.next = id
				_, err := s.create.Execute(ctx, dto.CreateOrderInput{CustomerID: "alice", Items: []dto.OrderLine{{SKU: "SKU-1", Quantity: 1}}})
				return err
			}, "", "PENDING"},
		{"ConfirmOrder", false, func(s *hstack, id string) { s.newOrder(asCustomer("alice"), id, "alice") },
			func(s *hstack, ctx context.Context, id string) error {
				_, err := s.confirm.Execute(ctx, dto.ConfirmOrderInput{OrderID: id})
				return err
			}, "PENDING", "CONFIRMED"},
		{"CancelOrder", false, confirmed,
			func(s *hstack, ctx context.Context, id string) error {
				_, err := s.cancel.Execute(ctx, dto.CancelOrderInput{OrderID: id, Source: dto.CancelSourceRPC})
				return err
			}, "CONFIRMED", "CANCELLED"},
		{"CompleteOrder", false, confirmed,
			func(s *hstack, ctx context.Context, id string) error {
				_, err := s.complete.Execute(ctx, dto.CompleteOrderInput{OrderID: id})
				return err
			}, "CONFIRMED", "COMPLETED"},
		{"ApplyPaymentCaptured", true, confirmed,
			func(s *hstack, ctx context.Context, id string) error {
				_, err := s.captured.Execute(ctx, dto.PaymentCapturedInput{OrderID: id})
				return err
			}, "CONFIRMED", "COMPLETED"},
		{"ApplyPaymentExpired", true, confirmed,
			func(s *hstack, ctx context.Context, id string) error {
				_, err := s.expired.Execute(ctx, dto.PaymentExpiredInput{OrderID: id})
				return err
			}, "CONFIRMED", "CANCELLED"},
	}

	for i, o := range ops {
		t.Run(o.name, func(t *testing.T) {
			s := newStack(t, db, stackOpts{requirePayment: o.requirePay})
			s.clock.set(day(10))
			id := "o-lock-" + string(rune('a'+i))
			o.prepare(s, id)
			rowsBefore, outboxBefore := len(s.rows(id)), outboxRows(s)

			holder := db.Begin()
			if holder.Error != nil {
				t.Fatal(holder.Error)
			}
			locked := true
			defer func() {
				if locked {
					_ = holder.Rollback().Error
				}
			}()
			if err := holder.Exec(`LOCK TABLE ordering.order_history IN ACCESS EXCLUSIVE MODE`).Error; err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(asCustomer("alice"), 700*time.Millisecond)
			start := time.Now()
			err := o.run(s, ctx, id)
			cancel()
			if err == nil {
				t.Fatalf("%s succeeded while order_history was locked: its write path does not wait on the table", o.name)
			}
			if took := time.Since(start); took < 500*time.Millisecond {
				t.Errorf("%s failed after %s: that is not a wait that ran into the deadline (%v)", o.name, took, err)
			}

			// The whole transaction rolled back: the order, its history and the outbox are as before.
			if got := status(s, id); got != o.wantBefore {
				t.Errorf("order status after the timed-out %s = %q, want %q", o.name, got, o.wantBefore)
			}
			if got := outboxRows(s); got != outboxBefore {
				t.Errorf("outbox rows after the timed-out %s = %d, want %d (an event row survived the rollback)", o.name, got, outboxBefore)
			}

			// Control: the same call, lock released.
			if err := holder.Rollback().Error; err != nil {
				t.Fatal(err)
			}
			locked = false
			if rows := len(s.rows(id)); rows != rowsBefore {
				t.Fatalf("history rows after the timed-out %s = %d, want %d", o.name, rows, rowsBefore)
			}
			if err := o.run(s, asCustomer("alice"), id); err != nil {
				t.Fatalf("%s after the lock is gone: %v", o.name, err)
			}
			if got := status(s, id); got != o.wantAfter {
				t.Errorf("order status after the control %s = %q, want %q", o.name, got, o.wantAfter)
			}
			if got := outboxRows(s); got <= outboxBefore {
				t.Errorf("outbox rows after the control %s = %d, want more than %d", o.name, got, outboxBefore)
			}
		})
	}

	t.Run("GetOrderHistory", func(t *testing.T) {
		s := newStack(t, db, stackOpts{})
		s.clock.set(day(10))
		s.newOrder(asCustomer("alice"), "o-lock-read", "alice")
		uc := s.historyUC()

		holder := db.Begin()
		if holder.Error != nil {
			t.Fatal(holder.Error)
		}
		locked := true
		defer func() {
			if locked {
				_ = holder.Rollback().Error
			}
		}()
		if err := holder.Exec(`LOCK TABLE ordering.order_history IN ACCESS EXCLUSIVE MODE`).Error; err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(asCustomer("alice"), 700*time.Millisecond)
		start := time.Now()
		_, err := uc.Execute(ctx, dto.GetOrderHistoryInput{OrderID: "o-lock-read"})
		cancel()
		if err == nil || time.Since(start) < 500*time.Millisecond {
			t.Fatalf("GetOrderHistory with the table locked = %v after %s, want a wait that ran into the deadline", err, time.Since(start))
		}

		if err := holder.Rollback().Error; err != nil {
			t.Fatal(err)
		}
		locked = false
		if got := readHistory(t, asCustomer("alice"), uc, "o-lock-read"); len(got.Entries) != 1 {
			t.Fatalf("GetOrderHistory after the lock is gone = %+v, want the CREATED entry", got.Entries)
		}
	})
}
