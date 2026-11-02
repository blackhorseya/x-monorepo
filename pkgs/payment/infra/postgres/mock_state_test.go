package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	paymentbc "github.com/blackhorseya/x-monorepo/pkgs/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock/storetest"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

// `serve`, `consume` and `expire` are three processes that share one database and nothing else
// (docs/payment.md §3「已知限制」). These tests play them as three independent mock providers, each built
// the way the composition root builds one, over one Postgres: what one opens, pays or refunds, the
// others must see, and a fifth built afterwards (a restart) must still see it.

// mockPaymentConfig is a payment.* configuration the provider accepts.
func mockPaymentConfig() config.Payment {
	return config.Payment{
		Provider: mock.Name, MockCheckoutTTL: 30 * time.Minute, MockRedeliveryWindow: 72 * time.Hour,
		MockWebhookSecret: "shared-secret", ReconcileWindow: 24 * time.Hour, Window: 30 * time.Minute,
		WebhookRetention: 720 * time.Hour, ExpireInterval: time.Minute, ExpireBatch: 50,
	}
}

// newInstance is one process's provider over the shared database.
func newInstance(t *testing.T, db *gorm.DB) *mock.Provider {
	t.Helper()
	p, err := paymentbc.ProvidePaymentProvider(mockPaymentConfig(), db)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := p.(*mock.Provider)
	if !ok {
		t.Fatalf("payment.provider mock built a %T", p)
	}

	return m
}

func openCheckout(t *testing.T, p port.PaymentProvider, n int64) dto.ProviderCheckoutOutput {
	t.Helper()
	co, err := p.CreateCheckout(context.Background(), dto.ProviderCheckoutInput{
		PaymentID: "pay-" + t.Name(), OrderID: "ord-" + t.Name(), Amount: dto.MoneyOutput{Amount: n, Currency: "TWD"},
	})
	if err != nil {
		t.Fatalf("open a checkout: %v", err)
	}

	return co
}

func TestMockState_ThreeProcessesShareCheckoutPayAndRefund(t *testing.T) {
	db := testDB(t)
	c := context.Background()
	serve, consume, expire := newInstance(t, db), newInstance(t, db), newInstance(t, db)

	co := openCheckout(t, serve, 1000)
	// the others know the checkout that serve opened, and that nobody has paid yet
	for name, p := range map[string]*mock.Provider{"consume": consume, "expire": expire} {
		got, err := p.GetPayment(c, co.Ref)
		if err != nil || got.Paid {
			t.Fatalf("%s asks about serve's checkout before it is paid: %+v, %v (want: known, not paid)", name, got, err)
		}
	}

	// the customer pays at serve's mock; expire finds the money, consume refunds it
	if _, err := serve.Pay(co.Ref, twd(t, 1000)); err != nil {
		t.Fatal(err)
	}
	got, err := expire.GetPayment(c, co.Ref)
	if err != nil || !got.Paid || got.Amount != (dto.MoneyOutput{Amount: 1000, Currency: "TWD"}) {
		t.Fatalf("expire after serve was paid: %+v, %v", got, err)
	}
	for range 3 {
		if err = consume.Refund(c, dto.ProviderRefundInput{IdempotencyKey: "pay-1", Ref: co.Ref, Amount: dto.MoneyOutput{Amount: 1000, Currency: "TWD"}}); err != nil {
			t.Fatalf("consume refunds a payment serve's mock took: %v", err)
		}
	}
	for name, p := range map[string]*mock.Provider{"serve": serve, "consume": consume, "expire": expire} {
		if n := p.Refunds(co.Ref); n != 1 {
			t.Fatalf("%s sees %d refunds, want 1 (the same key three times, from another process)", name, n)
		}
	}
	if err = expire.CancelCheckout(c, co.Ref); err != nil || !serve.Cancelled(co.Ref) {
		t.Fatalf("cancel from expire: %v, serve sees cancelled=%v", err, serve.Cancelled(co.Ref))
	}

	// a restart: a provider built after everything above still has it
	restarted := newInstance(t, db)
	if got, err = restarted.GetPayment(c, co.Ref); err != nil || !got.Paid || restarted.Refunds(co.Ref) != 1 {
		t.Fatalf("after a restart: %+v, %v, refunds %d", got, err, restarted.Refunds(co.Ref))
	}
}

// The T6 / T10 stretch of the flow through the real use cases: serve opens the checkout, the customer
// pays, expire's sweep finds the money (T6 for a PENDING payment, T10 for one whose order had ended) and
// consume's refund completes — three providers, one database.
func TestMockState_SweepAndRefundUseCasesOnSharedState(t *testing.T) {
	e := newSagaEnv(t)
	db := e.db
	c := context.Background()
	serve, consume, expire := newInstance(t, db), newInstance(t, db), newInstance(t, db)
	repo := postgres.NewPaymentRepository(db)
	for _, id := range []string{"mockstate-1", "mockstate-2"} {
		if err := db.Exec(`DELETE FROM payment.payments WHERE id = ?`, id).Error; err != nil {
			t.Fatal(err)
		}
	}

	start := usecase.NewStartPayment(repo, serve, e.tx, fixedClock{})
	var refs [2]string
	for i, id := range []string{"mockstate-1", "mockstate-2"} {
		if err := repo.Save(c, newPayment(t, id, "ord-"+id)); err != nil {
			t.Fatal(err)
		}
		out, err := start.Execute(c, dto.StartPaymentInput{OrderID: "ord-" + id, CustomerID: "cust"})
		if err != nil {
			t.Fatalf("start %s: %v", id, err)
		}
		p, err := repo.FindByID(c, payment.ID(id))
		if err != nil || out == nil {
			t.Fatalf("find %s: %v", id, err)
		}
		refs[i] = p.ProviderRef()
		if _, err = serve.Pay(refs[i], twd(t, 1000)); err != nil {
			t.Fatal(err)
		}
	}
	// the second payment's order ended while the customer was paying
	cancelled, err := repo.FindByID(c, "mockstate-2")
	if err != nil {
		t.Fatal(err)
	}
	cancelled.OrderCancelled(t0.Add(time.Minute))
	if err = repo.Save(c, cancelled); err != nil {
		t.Fatal(err)
	}

	late := fixedLater{at: t0.Add(time.Hour)}
	sweep := usecase.NewExpireSweep(repo, e.hooks, expire, e.ev, e.al, e.tx, late, dto.SweepConfig{ReconcileWindow: 24 * time.Hour, Interval: time.Minute, Batch: 50})
	out, err := sweep.Execute(c, dto.Ack{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if out.QueryFailures != 0 || out.Captured != 2 {
		t.Fatalf("sweep through expire's mock: %+v (want both payments found paid; a mock that does not know serve's checkouts answers 'unavailable')", *out)
	}
	one, _ := repo.FindByID(c, "mockstate-1")
	two, _ := repo.FindByID(c, "mockstate-2")
	if one.Status() != payment.StatusCaptured || two.Status() != payment.StatusRefunding {
		t.Fatalf("after the sweep: %s / %s, want CAPTURED / REFUNDING", one.Status(), two.Status())
	}

	refund := usecase.NewProcessRefund(repo, consume, e.ev, e.tx, late)
	for range 2 {
		if _, err = refund.Execute(c, dto.RefundInput{PaymentID: "mockstate-2"}); err != nil {
			t.Fatalf("refund through consume's mock: %v", err)
		}
	}
	done, _ := repo.FindByID(c, "mockstate-2")
	if done.Status() != payment.StatusRefunded || serve.Refunds(refs[1]) != 1 {
		t.Fatalf("after the refund: %s, refunds seen by serve %d", done.Status(), serve.Refunds(refs[1]))
	}
}

type fixedLater struct{ at time.Time }

func (f fixedLater) Now() time.Time { return f.at }

// The database, not a lock in one process, decides a race on one ref: the same key from many processes
// refunds once, different keys count each, and a refund racing the payment either finds it paid or is
// refused — it never leaves a refund of money that was not paid.
func TestMockState_ConcurrentPayAndRefundOnOneRef(t *testing.T) {
	db := testDB(t)
	c := context.Background()
	opener := newInstance(t, db)
	const procs = 6

	t.Run("same key from many processes refunds once", func(t *testing.T) {
		co := openCheckout(t, opener, 500)
		if _, err := opener.Pay(co.Ref, twd(t, 500)); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make(chan error, procs)
		for range procs {
			p := newInstance(t, db)
			wg.Add(1)
			go func() { // fire-and-forget: joined by wg below
				defer wg.Done()
				errs <- p.Refund(c, dto.ProviderRefundInput{IdempotencyKey: "same", Ref: co.Ref, Amount: dto.MoneyOutput{Amount: 500, Currency: "TWD"}})
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("a refund with the same key: %v", err)
			}
		}
		if n := opener.Refunds(co.Ref); n != 1 {
			t.Fatalf("refunds = %d, want 1", n)
		}
	})

	t.Run("different keys each count", func(t *testing.T) {
		co := openCheckout(t, opener, 500)
		if _, err := opener.Pay(co.Ref, twd(t, 500)); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for i := range procs {
			p, key := newInstance(t, db), string(rune('a'+i))
			wg.Add(1)
			go func() { // fire-and-forget: joined by wg below
				defer wg.Done()
				_ = p.Refund(c, dto.ProviderRefundInput{IdempotencyKey: key, Ref: co.Ref, Amount: dto.MoneyOutput{Amount: 500, Currency: "TWD"}})
			}()
		}
		wg.Wait()
		if n := opener.Refunds(co.Ref); n != procs {
			t.Fatalf("refunds = %d, want %d", n, procs)
		}
	})

	t.Run("a refund racing the payment is paid-or-refused, never a refund of nothing", func(t *testing.T) {
		refundedRuns, refusedRuns := 0, 0
		for i := range 300 {
			stagger := time.Duration(i%20) * 150 * time.Microsecond // sweeps the refund across the payment's write
			co := openCheckout(t, opener, 500)
			payer, refunder := newInstance(t, db), newInstance(t, db)
			start := make(chan struct{})
			var wg sync.WaitGroup
			var refundErr error
			wg.Add(2)
			go func() { // fire-and-forget: joined by wg below
				defer wg.Done()
				<-start
				_, _ = payer.Pay(co.Ref, twd(t, 500))
			}()
			go func() { // fire-and-forget: joined by wg below
				defer wg.Done()
				<-start
				time.Sleep(stagger)
				refundErr = refunder.Refund(c, dto.ProviderRefundInput{IdempotencyKey: "race", Ref: co.Ref, Amount: dto.MoneyOutput{Amount: 500, Currency: "TWD"}})
			}()
			close(start)
			wg.Wait()

			got, err := opener.GetPayment(c, co.Ref)
			if err != nil {
				t.Fatal(err)
			}
			if refunds := opener.Refunds(co.Ref); refunds > 0 && !got.Paid {
				t.Fatalf("a refund was recorded for a checkout that is not paid (refund err %v)", refundErr)
			}
			if refundErr != nil && !errors.Is(refundErr, port.ErrProviderUnavailable) {
				t.Fatalf("a refused refund must be the fixed unavailable error: %v", refundErr)
			}
			if refundErr == nil {
				refundedRuns++
			} else {
				refusedRuns++
			}
		}
		// positive control: the race really went both ways, or the checks above saw only one order
		if refundedRuns == 0 || refusedRuns == 0 {
			t.Fatalf("the race went one way only (refunded %d, refused %d of 300): the checks above proved nothing", refundedRuns, refusedRuns)
		}
	})
}

// mockStoreFor is a store over the shared database with the mock tables emptied.
func mockStoreFor(t *testing.T) mock.Store {
	t.Helper()
	db := testDB(t)
	clean := func() {
		if err := db.Exec(`TRUNCATE payment.mock_refunds, payment.mock_checkouts`).Error; err != nil {
			t.Errorf("truncate mock tables: %v", err)
		}
	}
	clean()
	t.Cleanup(clean)

	return postgres.NewMockStore(db)
}

// The Postgres store keeps the contract the in-memory store keeps.
func TestMockStore_Contract(t *testing.T) {
	storetest.Run(t, mockStoreFor)
}

func TestMockStore_Faults(t *testing.T) {
	storetest.RunFaults(t, mockStoreFor)
}
