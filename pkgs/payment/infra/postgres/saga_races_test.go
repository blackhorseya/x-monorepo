package postgres_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"
	"gorm.io/gorm"

	paymentbc "github.com/blackhorseya/x-monorepo/pkgs/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/adapters/events"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/alerts"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/system"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// These tests run the real use cases, decorators, transactor, outbox and repositories on a real Postgres,
// and steer only the timing (a gate before one write, a clock). They pin the three timing holes the
// review of #387 found: each one leaves every handler "successful" while the money or the cancellation
// is gone, so a unit test with a fake repository cannot see them.

type sagaEnv struct {
	db     *gorm.DB
	repo   payment.Repository
	outs   payment.OutcomeRepository
	hooks  payment.WebhookRepository
	tx     port.Transactor
	ev     port.EventPublisher
	al     port.Alerts
	rec    *usecasex.Recorder
	logger *slog.Logger
}

func newSagaEnv(t *testing.T) *sagaEnv {
	t.Helper()
	db := testDB(t)
	clean := func() {
		if err := db.Exec("TRUNCATE payment.order_outcomes, payment.webhook_events").Error; err != nil {
			t.Errorf("truncate saga tables: %v", err)
		}
	}
	clean()
	t.Cleanup(clean)
	logger := slog.New(slog.DiscardHandler)
	meter := noop.NewMeterProvider()
	rec, err := usecasex.NewRecorder(meter)
	if err != nil {
		t.Fatal(err)
	}
	al, err := alerts.New(logger, meter)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := eventbus.NewPostgresPublisher(db, logger)
	if err != nil {
		t.Fatal(err)
	}

	return &sagaEnv{db: db, repo: postgres.NewPaymentRepository(db), outs: postgres.NewOutcomeRepository(db), hooks: postgres.NewWebhookRepository(db),
		tx: postgres.NewTransactor(db), ev: events.NewPublisher(pub), al: al, rec: rec, logger: logger}
}

// gatedRepo stops a Save of a never-saved payment until released, so a test decides what happens
// between "the use case decided" and "the payment row exists".
type gatedRepo struct {
	payment.Repository
	reached chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (r *gatedRepo) Save(c context.Context, p *payment.Payment) error {
	r.once.Do(func() { close(r.reached) })
	select {
	case <-r.resume:
	case <-c.Done():
		return c.Err()
	}

	return r.Repository.Save(c, p)
}

// OrderConfirmed and OrderCancelled for an order with no payment yet, interleaved so that the
// cancellation runs entirely inside the confirmation's decide-to-insert gap. Whatever the order they
// finish in, the stored payment must be cancelled: both handlers ack, so nothing would ever repair it.
func TestRace_ConfirmedAndCancelledWithoutAPaymentYet(t *testing.T) {
	e := newSagaEnv(t)
	c := t.Context()
	orderID, err := system.NewIDGenerator().NewID()
	if err != nil {
		t.Fatal(err)
	}
	gate := &gatedRepo{Repository: e.repo, reached: make(chan struct{}), resume: make(chan struct{})}
	clock := system.NewClock()
	confirm := paymentbc.ProvideRecordOrderConfirmed(gate, e.outs, system.NewIDGenerator(), clock, e.al, config.Payment{Window: 30 * time.Minute}, e.tx, nil, e.rec)
	cancel := paymentbc.ProvideRecordOrderCancelled(e.repo, e.outs, e.ev, clock, mock.New(mock.Options{}), e.logger, e.tx, nil, e.rec)

	release := sync.OnceFunc(func() { close(gate.resume) })
	t.Cleanup(release)
	confirmed := make(chan error, 1)
	go func() {
		_, err := confirm.Execute(c, dto.OrderConfirmedInput{OrderID: orderID, CustomerID: "cust", Total: &dto.MoneyOutput{Amount: 1000, Currency: "TWD"}})
		confirmed <- err
	}()
	select {
	case <-gate.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("fixture: the confirmation never reached its insert")
	}

	cancelled := make(chan error, 1)
	go func() {
		_, err := cancel.Execute(c, dto.OrderEndedInput{OrderID: orderID})
		cancelled <- err
	}()
	// Let the cancellation finish if nothing stops it; if it is held back by the confirmation, let the
	// confirmation go on after a pause that is long enough for a cancellation to have run.
	var cancelErr error
	select {
	case cancelErr = <-cancelled:
	case <-time.After(750 * time.Millisecond):
		release()
		cancelErr = <-cancelled
	}
	release()
	if cancelErr != nil {
		t.Fatalf("cancel: %v", cancelErr)
	}
	if err = <-confirmed; err != nil {
		t.Fatalf("confirm: %v", err)
	}

	p, err := e.repo.FindByOrderID(c, orderID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status() != payment.StatusCancelled || p.Outcome() != payment.OutcomeCancelled {
		t.Fatalf("both handlers acked but the cancellation was lost: payment=%s/%s", p.Status(), p.Outcome())
	}
}

type sweepClock struct{ now time.Time }

func (c *sweepClock) Now() time.Time { return c.now }

type countingProvider struct {
	*mock.Provider
	mu    sync.Mutex
	calls int
}

func (p *countingProvider) GetPayment(c context.Context, ref string) (dto.ProviderPaymentOutput, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()

	return p.Provider.GetPayment(c, ref)
}

// More reconcilable payments than one batch: the paid one is behind an unpaid one. A healthy provider
// answers every query, and the paid cancellation must be found within its window all the same.
func TestSweep_ReconcileReachesEveryPaymentWhenEligibleExceedsTheBatch(t *testing.T) {
	e := newSagaEnv(t)
	c := t.Context()
	clock := &sweepClock{now: t0}
	prov := &countingProvider{Provider: mock.New(mock.Options{Now: clock.Now})}

	var refs []string
	for n, ord := range []string{"starve-unpaid", "starve-paid"} {
		p := newPayment(t, "pay-"+ord, ord)
		co, err := prov.CreateCheckout(c, dto.ProviderCheckoutInput{PaymentID: p.ID().String(), Amount: dto.MoneyOutput{Amount: 1000, Currency: "TWD"}})
		if err != nil {
			t.Fatal(err)
		}
		if err = p.AttachCheckout("mock", co.Ref, co.URL, co.ExpiresAt, clock.Now()); err != nil {
			t.Fatal(err)
		}
		p.OrderCancelled(clock.Now())
		if err = e.repo.Save(c, p); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, co.Ref)
		if n == 0 {
			clock.now = clock.now.Add(time.Minute)
		}
	}
	if _, err := prov.Pay(refs[1], twd(t, 1000)); err != nil {
		t.Fatal(err)
	}

	sweep := usecase.NewExpireSweep(e.repo, e.hooks, prov, e.ev, e.al, e.tx, clock,
		dto.SweepConfig{Batch: 1, ReconcileWindow: 24 * time.Hour, Interval: 15 * time.Minute})
	for range 12 { // three hours: far inside the 24h window
		clock.now = clock.now.Add(15 * time.Minute)
		if _, err := sweep.Execute(c, dto.Ack{}); err != nil {
			t.Fatal(err)
		}
	}
	if prov.calls == 0 {
		t.Fatal("fixture: no query reached the provider")
	}

	p, err := e.repo.FindByOrderID(c, "starve-paid")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status() != payment.StatusRefunding {
		t.Fatalf("the paid cancellation behind an unpaid one was never reconciled: status=%s after %d queries", p.Status(), prov.calls)
	}
}

// Pay at the first checkout, reopen a second, cancel, then the first checkout's notification arrives:
// the money is real and the order is cancelled, so the payment must go to REFUNDING, not be dropped as
// an orphan because its reference is no longer the current one.
func TestWebhook_AnOldCheckoutsPaymentStillFindsItsOrderAfterAReopen(t *testing.T) {
	e := newSagaEnv(t)
	c := t.Context()
	clock := &sweepClock{now: t0}
	prov := mock.New(mock.Options{Now: clock.Now})

	p := newPayment(t, "old-ref-pay", "old-ref-order")
	if err := e.repo.Save(c, p); err != nil {
		t.Fatal(err)
	}
	start := paymentbc.ProvideStartPayment(e.repo, prov, e.tx, clock, e.rec)
	if _, err := start.Execute(c, dto.StartPaymentInput{OrderID: p.OrderID(), CustomerID: "cust"}); err != nil {
		t.Fatal(err)
	}
	p, err := e.repo.FindByOrderID(c, p.OrderID())
	if err != nil {
		t.Fatal(err)
	}
	old := p.ProviderRef()
	clock.now = clock.now.Add(14 * time.Minute) // still inside the first checkout's validity
	delivery, err := prov.Pay(old, p.Amount())
	if err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(2 * time.Minute) // the first URL has expired; the buyer reopens
	if _, err = start.Execute(c, dto.StartPaymentInput{OrderID: p.OrderID(), CustomerID: "cust"}); err != nil {
		t.Fatal(err)
	}
	if p, err = e.repo.FindByOrderID(c, p.OrderID()); err != nil || p.ProviderRef() == old {
		t.Fatalf("fixture: the reference did not change: %v", err)
	}

	cancel := paymentbc.ProvideRecordOrderCancelled(e.repo, e.outs, e.ev, clock, prov, e.logger, e.tx, nil, e.rec)
	if _, err = cancel.Execute(c, dto.OrderEndedInput{OrderID: p.OrderID()}); err != nil {
		t.Fatal(err)
	}
	receive := paymentbc.ProvideReceiveWebhook(e.hooks, prov, e.ev, e.tx, clock, e.rec)
	if _, err = receive.Execute(c, dto.WebhookInput{Body: delivery.Body, Signature: delivery.Signature}); err != nil {
		t.Fatal(err)
	}
	decoded, err := prov.VerifyWebhook(delivery.Body, delivery.Signature)
	if err != nil {
		t.Fatal(err)
	}
	process := paymentbc.ProvideProcessWebhook(e.repo, e.hooks, e.ev, e.al, clock, e.tx, nil, e.rec)
	if _, err = process.Execute(c, dto.WebhookReceivedInput{Provider: "mock", EventID: decoded.ID}); err != nil {
		t.Fatal(err)
	}

	if p, err = e.repo.FindByOrderID(c, p.OrderID()); err != nil {
		t.Fatal(err)
	}
	stored, err := e.hooks.Find(c, "mock", decoded.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status() != payment.StatusRefunding {
		t.Fatalf("the old checkout's payment was not refunded: payment=%s, notification processed=%v", p.Status(), stored.Processed())
	}
}
