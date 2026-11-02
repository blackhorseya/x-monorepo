package postgres_test

import (
	"context"
	"errors"
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

// endings are the two events that end an order before its payment exists: OrderCancelled (T15 -> T14) and
// OrderCompleted (T15 -> T14 with COMPLETED_UNPAID, docs/payment.md §3). Each race below runs for both: the payment must end
// CANCELLED with the outcome the event names, in whichever order the handlers interleave — both ack, so nothing
// would repair a loss.
type ending struct {
	name    string
	outcome payment.Outcome
	handle  func(e *sagaEnv, repo payment.Repository, outs payment.OutcomeRepository, clock port.Clock) func(context.Context, string) error
}

var endings = []ending{
	{"cancelled", payment.OutcomeCancelled, func(e *sagaEnv, repo payment.Repository, outs payment.OutcomeRepository, clock port.Clock) func(context.Context, string) error {
		uc := paymentbc.ProvideRecordOrderCancelled(repo, outs, e.ev, clock, mock.New(mock.Options{}), e.logger, e.tx, nil, e.rec)
		return func(c context.Context, id string) error {
			_, err := uc.Execute(c, dto.OrderEndedInput{OrderID: id})
			return err
		}
	}},
	{"completed", payment.OutcomeCompletedUnpaid, func(e *sagaEnv, repo payment.Repository, outs payment.OutcomeRepository, clock port.Clock) func(context.Context, string) error {
		uc := paymentbc.ProvideRecordOrderCompleted(repo, outs, e.ev, clock, mock.New(mock.Options{}), e.logger, e.tx, nil, e.rec)
		return func(c context.Context, id string) error {
			_, err := uc.Execute(c, dto.OrderEndedInput{OrderID: id})
			return err
		}
	}},
}

// OrderConfirmed and OrderCancelled for an order with no payment yet, interleaved so that the
// cancellation runs entirely inside the confirmation's decide-to-insert gap. Whatever the order they
// finish in, the stored payment must be cancelled: both handlers ack, so nothing would ever repair it.
func TestRace_ConfirmedAndEndedWithoutAPaymentYet(t *testing.T) {
	for _, end := range endings {
		t.Run(end.name, func(t *testing.T) { raceConfirmedThenEnded(t, end) })
	}
}

func raceConfirmedThenEnded(t *testing.T, end ending) {
	t.Helper()
	e := newSagaEnv(t)
	c := t.Context()
	orderID, err := system.NewIDGenerator().NewID()
	if err != nil {
		t.Fatal(err)
	}
	gate := &gatedRepo{Repository: e.repo, reached: make(chan struct{}), resume: make(chan struct{})}
	clock := system.NewClock()
	confirm := paymentbc.ProvideRecordOrderConfirmed(gate, e.outs, system.NewIDGenerator(), clock, e.al, config.Payment{Window: 30 * time.Minute}, e.tx, nil, e.rec)
	cancel := end.handle(e, e.repo, e.outs, clock)

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
	go func() { cancelled <- cancel(c, orderID) }()
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
	if p.Status() != payment.StatusCancelled || p.Outcome() != end.outcome {
		t.Fatalf("both handlers acked but the %s event was lost: payment=%s/%s, want %s/%s", end.name, p.Status(), p.Outcome(), payment.StatusCancelled, end.outcome)
	}
}

// gatedOutcomes stops Record (the cancellation's write when no payment exists) until released: the other
// order of the same race, the cancellation first and the confirmation arriving while it is half done.
type gatedOutcomes struct {
	payment.OutcomeRepository
	reached chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (r *gatedOutcomes) Record(c context.Context, orderID string, o payment.Outcome, at time.Time) error {
	r.once.Do(func() { close(r.reached) })
	select {
	case <-r.resume:
	case <-c.Done():
		return c.Err()
	}

	return r.OutcomeRepository.Record(c, orderID, o, at)
}

func TestRace_EndedThenConfirmedWithoutAPaymentYet(t *testing.T) {
	for _, end := range endings {
		t.Run(end.name, func(t *testing.T) { raceEndedThenConfirmed(t, end) })
	}
}

func raceEndedThenConfirmed(t *testing.T, end ending) {
	t.Helper()
	e := newSagaEnv(t)
	c := t.Context()
	orderID, err := system.NewIDGenerator().NewID()
	if err != nil {
		t.Fatal(err)
	}
	gate := &gatedOutcomes{OutcomeRepository: e.outs, reached: make(chan struct{}), resume: make(chan struct{})}
	clock := system.NewClock()
	cancel := end.handle(e, e.repo, gate, clock)
	confirm := paymentbc.ProvideRecordOrderConfirmed(e.repo, e.outs, system.NewIDGenerator(), clock, e.al, config.Payment{Window: 30 * time.Minute}, e.tx, nil, e.rec)

	release := sync.OnceFunc(func() { close(gate.resume) })
	t.Cleanup(release)
	cancelled := make(chan error, 1)
	go func() { cancelled <- cancel(c, orderID) }()
	select {
	case <-gate.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("fixture: the cancellation never reached its write")
	}

	confirmed := make(chan error, 1)
	go func() {
		_, err := confirm.Execute(c, dto.OrderConfirmedInput{OrderID: orderID, CustomerID: "cust", Total: &dto.MoneyOutput{Amount: 1000, Currency: "TWD"}})
		confirmed <- err
	}()
	var confirmErr error
	select {
	case confirmErr = <-confirmed:
	case <-time.After(750 * time.Millisecond):
		release()
		confirmErr = <-confirmed
	}
	release()
	if confirmErr != nil {
		t.Fatalf("confirm: %v", confirmErr)
	}
	if err = <-cancelled; err != nil {
		t.Fatalf("cancel: %v", err)
	}

	p, err := e.repo.FindByOrderID(c, orderID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status() != payment.StatusCancelled || p.Outcome() != end.outcome {
		t.Fatalf("both handlers acked but the %s event was lost: payment=%s/%s, want %s/%s", end.name, p.Status(), p.Outcome(), payment.StatusCancelled, end.outcome)
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

// Pay at the checkout, ask to start the payment again once a shorter page would have ended, cancel, then
// the notification of that payment arrives late: there is ONE checkout for the whole window, so the second
// call hands out the same reference and the late notification finds its payment. The money is real and the
// order is cancelled, so it goes to REFUNDING, not dropped as an orphan.
func TestWebhook_ALateNotificationFindsItsPaymentAfterTheBuyerStartsAgain(t *testing.T) {
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
	clock.now = clock.now.Add(2 * time.Minute) // a page of the old 15 minutes would have ended; the buyer asks again
	if _, err = start.Execute(c, dto.StartPaymentInput{OrderID: p.OrderID(), CustomerID: "cust"}); err != nil {
		t.Fatal(err)
	}
	if p, err = e.repo.FindByOrderID(c, p.OrderID()); err != nil || p.ProviderRef() != old {
		t.Fatalf("StartPayment opened a second checkout (%q after %q): %v", p.ProviderRef(), old, err)
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

// failingFor cannot answer about one checkout.
type failingFor struct {
	port.PaymentProvider
	ref string
}

func (f failingFor) GetPayment(c context.Context, ref string) (dto.ProviderPaymentOutput, error) {
	if ref == f.ref {
		return dto.ProviderPaymentOutput{}, port.ErrProviderUnavailable
	}

	return f.PaymentProvider.GetPayment(c, ref)
}

// The due PENDING list on Postgres: a payment whose query keeps failing stays PENDING, and must not keep
// the one behind it from being settled when the batch is one.
func TestSweep_AFailingQueryDoesNotHoldTheFrontOfTheDueListOnPostgres(t *testing.T) {
	e := newSagaEnv(t)
	c := t.Context()
	clock := &sweepClock{now: t0}
	prov := mock.New(mock.Options{Now: clock.Now})

	var refs []string
	for n, ord := range []string{"due-a", "due-b"} {
		p, err := payment.New(payment.NewParams{ID: payment.ID("pay-" + ord), OrderID: ord, CustomerID: "cust", Total: twd(t, 1000), ExpiresAt: t0.Add(time.Duration(n+1) * time.Minute)}, t0)
		if err != nil {
			t.Fatal(err)
		}
		co, err := prov.CreateCheckout(c, dto.ProviderCheckoutInput{PaymentID: p.ID().String(), Amount: dto.MoneyOutput{Amount: 1000, Currency: "TWD"}})
		if err != nil {
			t.Fatal(err)
		}
		if err = p.AttachCheckout("mock", co.Ref, co.URL, co.ExpiresAt, t0); err != nil {
			t.Fatal(err)
		}
		if err = e.repo.Save(c, p); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, co.Ref)
	}
	clock.now = t0.Add(time.Hour)

	sweep := usecase.NewExpireSweep(e.repo, e.hooks, failingFor{PaymentProvider: prov, ref: refs[0]}, e.ev, e.al, e.tx, clock,
		dto.SweepConfig{Batch: 1, ReconcileWindow: 24 * time.Hour, Interval: 15 * time.Minute})
	for range 2 {
		clock.now = clock.now.Add(15 * time.Minute)
		if _, err := sweep.Execute(c, dto.Ack{}); err != nil {
			t.Fatal(err)
		}
	}

	b, err := e.repo.FindByOrderID(c, "due-b")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status() != payment.StatusExpired {
		t.Fatalf("due-b behind a failing due-a: status = %s, want EXPIRED", b.Status())
	}
}

// A payment that EXISTS (PENDING, a checkout open), when its notification and the end of its order are processed at the
// same time (docs/payment.md §5: T3 against T7/T7', both writing the same row by version). One side decides on what it
// read and is held before it saves; the other runs to the end; the held one then loses on the version (ErrStale), is
// redelivered, reads again and decides on the new state. Whoever wins, what is left must be consistent:
//   - the order was cancelled: REFUNDING with exactly one refund request, whichever came first (T3->T10 or T7->T8->T10);
//   - the order was completed without this payment: the notification first -> CAPTURED, nothing to refund (the order
//     was paid); the end first -> the money arrives for an order that no longer needs it -> REFUNDING, one request.
func TestRace_AnExistingPaymentIsCapturedAndItsOrderEndsAtOnce(t *testing.T) {
	type want struct {
		status  payment.Status
		refunds int
	}
	for _, end := range endings {
		for _, captureHeld := range []bool{true, false} {
			name := end.name + "/the end wins"
			if !captureHeld {
				name = end.name + "/the capture wins"
			}
			t.Run(name, func(t *testing.T) {
				e := newSagaEnv(t)
				c := t.Context()
				clock := &sweepClock{now: t0.Add(time.Minute)} // inside the payment's window (newPayment is born at t0)
				prov := mock.New(mock.Options{Now: clock.Now})
				const refundTable = "payment.events_v1_paymentrefundrequested"
				if err := e.db.Exec("TRUNCATE " + refundTable).Error; err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = e.db.Exec("TRUNCATE " + refundTable).Error })

				p := newPayment(t, "race-existing-pay", "race-existing-order")
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
				d, err := prov.Pay(p.ProviderRef(), p.Amount())
				if err != nil {
					t.Fatal(err)
				}
				recv := paymentbc.ProvideReceiveWebhook(e.hooks, prov, e.ev, e.tx, clock, e.rec)
				if _, err = recv.Execute(c, dto.WebhookInput{Body: d.Body, Signature: d.Signature}); err != nil {
					t.Fatal(err)
				}

				// one side runs on a repository that holds its Save, the other on the plain one
				gate := &gatedRepo{Repository: e.repo, reached: make(chan struct{}), resume: make(chan struct{})}
				capRepo, endRepo := payment.Repository(e.repo), payment.Repository(e.repo)
				if captureHeld {
					capRepo = gate
				} else {
					endRepo = gate
				}
				capture := paymentbc.ProvideProcessWebhook(capRepo, e.hooks, e.ev, e.al, clock, e.tx, nil, e.rec)
				ended := end.handle(e, endRepo, e.outs, clock)
				runCapture := func() error {
					_, err := capture.Execute(c, dto.WebhookReceivedInput{Provider: mock.Name, EventID: d.EventID})
					return err
				}
				runEnded := func() error { return ended(c, p.OrderID()) }
				// a delivery that lost on the version is redelivered (the consumer's retry)
				retried := func(f func() error) func() error {
					return func() error {
						var err error
						for range 5 {
							if err = f(); !errors.Is(err, payment.ErrStale) {
								return err
							}
						}
						return err
					}
				}
				held, free := runEnded, runCapture
				if captureHeld {
					held, free = runCapture, runEnded
				}
				release := sync.OnceFunc(func() { close(gate.resume) })
				t.Cleanup(release)
				heldDone := make(chan error, 1)
				go func() { heldDone <- retried(held)() }()
				select {
				case <-gate.reached:
				case <-time.After(10 * time.Second):
					t.Fatal("fixture: the held side never reached its save")
				}
				if err = retried(free)(); err != nil {
					t.Fatalf("the free side: %v", err)
				}
				release()
				if err = <-heldDone; err != nil {
					t.Fatalf("the held side, redelivered: %v", err)
				}

				got, err := e.repo.FindByOrderID(c, p.OrderID())
				if err != nil {
					t.Fatal(err)
				}
				var refunds int
				if err = e.db.Raw("SELECT count(*) FROM " + refundTable).Scan(&refunds).Error; err != nil {
					t.Fatal(err)
				}
				w := want{payment.StatusRefunding, 1}
				if end.name == "completed" && !captureHeld {
					// the capture got in first: the order was paid for, nothing to give back
					w = want{payment.StatusCaptured, 0}
				}
				if got.Status() != w.status || refunds != w.refunds {
					t.Fatalf("payment=%s with %d refund request(s), want %s with %d", got.Status(), refunds, w.status, w.refunds)
				}
			})
		}
	}
}
