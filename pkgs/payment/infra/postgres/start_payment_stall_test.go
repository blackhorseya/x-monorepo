package postgres_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// #416: a StartPayment on the Postgres-backed mock waited out the caller's whole 3 s deadline and nothing
// said where. Here a session holds a lock the mock's statements queue behind (the stall, made on purpose),
// and the real driver's cancellation text — which is not a wrapped context error — must still be logged
// as the fixed class "deadline" and nothing else. The positive control is the same call without the lock.
func TestStartPayment_AStalledStoreIsLoggedAsADeadline(t *testing.T) {
	db := testDB(t)
	repo := postgres.NewPaymentRepository(db)
	// FaultsFromStore is what `payment-rpc mock_control` turns on in the kind release: the fault table is
	// read before every call, so the stalled statement may be that read or the insert — either is a deadline.
	prov := mock.New(mock.Options{CheckoutTTL: 30 * time.Minute, Now: fixedClock{}.Now, Store: postgres.NewMockStore(db), FaultsFromStore: true})
	start := usecase.NewStartPayment(repo, prov, postgres.NewTransactor(db), fixedClock{})

	var sink bytes.Buffer
	ctxOf := func(timeout time.Duration) (context.Context, context.CancelFunc) {
		c, cancel := context.WithTimeout(context.Background(), timeout)

		return contextx.WithLogger(c, slog.New(slog.NewJSONHandler(&sink, nil))), cancel
	}

	for _, table := range []string{"mock_checkouts", "mock_faults"} {
		t.Run("a lock on "+table, func(t *testing.T) {
			sink.Reset()
			// a fresh payment per case: a payment that already has its checkout never asks the provider again
			if err := db.Exec("TRUNCATE payment.payments").Error; err != nil {
				t.Fatal(err)
			}
			if err := repo.Save(context.Background(), newPayment(t, "pay-1", "ord-1")); err != nil {
				t.Fatal(err)
			}
			holder := db.Begin()
			if holder.Error != nil {
				t.Fatal(holder.Error)
			}
			released := false
			release := func() {
				if !released {
					released = true
					if err := holder.Rollback().Error; err != nil {
						t.Errorf("release the lock: %v", err)
					}
				}
			}
			defer release()
			if err := holder.Exec("LOCK TABLE " + postgres.Schema + "." + table + " IN ACCESS EXCLUSIVE MODE").Error; err != nil {
				t.Fatal(err)
			}

			c, cancel := ctxOf(300 * time.Millisecond)
			defer cancel()
			began := time.Now()
			_, err := start.Execute(c, startIn)
			took := time.Since(began)

			if !errors.Is(err, port.ErrProviderUnavailable) || err.Error() != port.ErrProviderUnavailable.Error() {
				t.Fatalf("err = %v, want exactly %v", err, port.ErrProviderUnavailable)
			}
			if took < 250*time.Millisecond || took > 5*time.Second {
				t.Fatalf("took %v: the call must wait for the lock until its 300ms deadline and then give up", took)
			}
			var rec map[string]any
			for _, line := range strings.Split(strings.TrimSpace(sink.String()), "\n") {
				var r map[string]any
				if json.Unmarshal([]byte(line), &r) == nil && r["msg"] == "payment provider failed" {
					rec = r
				}
			}
			if rec == nil || rec["class"] != "deadline" || rec["op"] != "create_checkout" || rec["order_id"] != "ord-1" {
				t.Fatalf("record = %v, want class=deadline op=create_checkout order_id=ord-1; log:\n%s", rec, sink.String())
			}
			if strings.Contains(strings.ToLower(sink.String()), "statement") || strings.Contains(sink.String(), "SQLSTATE") {
				t.Fatalf("the driver's text reached the log:\n%s", sink.String())
			}

			// positive control: with the lock gone the very same call succeeds, so the failure above was the lock
			release()
			c2, cancel2 := ctxOf(10 * time.Second)
			defer cancel2()
			out, err := start.Execute(c2, startIn)
			if err != nil || out.CheckoutURL == "" {
				t.Fatalf("after the lock is released: %+v, %v", out, err)
			}
		})
	}
}
