package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"google.golang.org/protobuf/proto"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	orderingpg "github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	paymentpg "github.com/blackhorseya/x-monorepo/pkgs/payment/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	commonv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/common/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/idgen"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

var groupSeq atomic.Int64

const (
	topicConfirmed = "ordering.v1.OrderConfirmed"
	topicCancelled = "ordering.v1.OrderCancelled"
	topicCompleted = "ordering.v1.OrderCompleted"
)

// consumeEnv is `payment-rpc consume` as main builds it (InitializeConsume: the real router, the real
// Postgres subscriber and outbox) on a real Postgres with both schemas migrated: ordering publishes the
// events payment consumes, so its tables must exist. Every table the consumer reads or writes is emptied
// before and after the test, and the consumer group is new each time, so no cursor of an earlier test is read.
type consumeEnv struct {
	t     *testing.T
	db    *gorm.DB
	pg    config.Postgres
	pub   message.Publisher
	group string
	prov  *mock.Provider
}

func newConsumeEnv(t *testing.T) *consumeEnv {
	t.Helper()
	pg := freshDB(t)
	if err := postgres.Up(context.Background(), pg, orderingpg.Migration()); err != nil {
		t.Fatalf("migrate ordering: %v", err)
	}
	tel, closeTel, err := otel.New(config.OTel{}, config.Log{Level: "error", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeTel)
	db, closeDB, err := postgres.NewDB(pg, tel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeDB)

	topics := []string{topicConfirmed, topicCancelled, topicCompleted, "payment.v1.PaymentCaptured", "payment.v1.PaymentExpired",
		"payment.v1.PaymentRefunded", "payment.v1.PaymentRefundRequested", "payment.v1.WebhookReceived", "payment.poison"}
	clean := func() {
		stmts := []string{"TRUNCATE payment.order_outcomes", "TRUNCATE payment.inbox", "TRUNCATE payment.webhook_events"}
		for _, topic := range topics {
			stmts = append(stmts, "TRUNCATE "+eventbus.TableName(topic)+", "+eventbus.OffsetsTableName(topic))
		}
		for _, q := range stmts {
			if err := db.Exec(q).Error; err != nil {
				t.Errorf("%s: %v", q, err)
			}
		}
	}
	clean()
	t.Cleanup(clean)

	pub, err := eventbus.NewPostgresPublisher(db, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	return &consumeEnv{t: t, db: db, pg: pg, pub: pub, group: fmt.Sprintf("test-%d-%d", time.Now().UnixNano(), groupSeq.Add(1)), prov: mock.New(mock.Options{})}
}

// start runs the consumer behind provider (nil = the env's mock) until the test ends.
func (e *consumeEnv) start(provider port.PaymentProvider) {
	e.t.Helper()
	if provider == nil {
		provider = e.prov
	}
	cfg := &config.AppConfig{
		Log:      config.Log{Level: "error", Format: "text"},
		Postgres: e.pg,
		Worker: config.Worker{
			ConsumerGroup: e.group, Broker: config.BrokerPostgres, StartFrom: config.StartFromBeginning,
			PollInterval: 20 * time.Millisecond, HandlerTimeout: 5 * time.Second,
		},
		Payment: config.Payment{Provider: "mock", Window: 30 * time.Minute, ReconcileWindow: 24 * time.Hour, MockCheckoutTTL: 15 * time.Minute},
	}
	consumer, cleanup, err := InitializeConsume(cfg, provider)
	if err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- consumer.Run(ctx) }()
	e.t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				e.t.Errorf("consumer stopped with %v", err)
			}
		case <-time.After(15 * time.Second):
			e.t.Error("the consumer did not stop within 15s of being cancelled")
		}
		cleanup()
	})
}

func (e *consumeEnv) publish(topic string, m proto.Message) {
	e.t.Helper()
	payload, err := proto.Marshal(m)
	if err != nil {
		e.t.Fatal(err)
	}
	if err = e.pub.Publish(topic, message.NewMessage(mustID(e.t), payload)); err != nil {
		e.t.Fatal(err)
	}
}

func mustID(t *testing.T) string {
	t.Helper()
	id, err := idgen.New()
	if err != nil {
		t.Fatal(err)
	}

	return id
}

// eventually waits (bounded) for cond; the failure names what it was waiting for.
func (e *consumeEnv) eventually(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (e *consumeEnv) repo() *paymentpg.PaymentRepository { return paymentpg.NewPaymentRepository(e.db) }

func (e *consumeEnv) payment(order string) (*payment.Payment, error) {
	return e.repo().FindByOrderID(context.Background(), order)
}

func (e *consumeEnv) mustPayment(order string) *payment.Payment {
	e.t.Helper()
	p, err := e.payment(order)
	if err != nil {
		e.t.Fatalf("payment of %s: %v", order, err)
	}

	return p
}

func (e *consumeEnv) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.Raw(q, args...).Scan(&n).Error; err != nil {
		e.t.Fatal(err)
	}

	return n
}

// consumed is how many messages of topic this test's consumer group has acknowledged.
func (e *consumeEnv) consumed(topic string) int {
	e.t.Helper()
	var n int
	q := fmt.Sprintf(`SELECT count(*) FROM %s t JOIN %s o ON o.consumer_group = ? WHERE t."offset" <= o.offset_acked`,
		eventbus.TableName(topic), eventbus.OffsetsTableName(topic))
	if err := e.db.Raw(q, e.group+".payment").Scan(&n).Error; err != nil {
		e.t.Fatal(err)
	}

	return n
}

func confirmed(order string, total *commonv1.Money) *orderingv1.OrderConfirmed {
	return &orderingv1.OrderConfirmed{OrderId: order, CustomerId: owner, Total: total}
}

func money(amount int64) *commonv1.Money {
	return &commonv1.Money{AmountMinor: amount, Currency: "TWD"}
}

// T1 / T13 on the real router: OrderConfirmed creates a payment, the same event again (a new message,
// the order is the key) creates nothing, OrderCancelled ends it.
func TestConsume_OrderLifecycle(t *testing.T) {
	e := newConsumeEnv(t)
	e.start(nil)

	e.publish(topicConfirmed, confirmed("ord-1", money(1500)))
	e.eventually("the payment of ord-1", func() bool { _, err := e.payment("ord-1"); return err == nil })
	p := e.mustPayment("ord-1")
	if p.Status() != payment.StatusPending || p.Amount().Amount() != 1500 || p.CustomerID() != owner || !p.ExpiresAt().After(time.Now()) {
		t.Fatalf("created payment = %s %v %s", p.Status(), p.Amount(), p.CustomerID())
	}

	e.publish(topicConfirmed, confirmed("ord-1", money(9999)))
	e.eventually("the repeat to be consumed", func() bool { return e.consumed(topicConfirmed) == 2 })
	if n := e.count(`SELECT count(*) FROM payment.payments WHERE order_id = 'ord-1'`); n != 1 {
		t.Fatalf("%d payments for ord-1 after a repeat", n)
	}
	if e.mustPayment("ord-1").Amount().Amount() != 1500 {
		t.Fatal("the repeat changed the amount")
	}

	e.publish(topicCancelled, &orderingv1.OrderCancelled{OrderId: "ord-1", CustomerId: owner})
	e.eventually("ord-1 to be CANCELLED", func() bool { return e.mustPayment("ord-1").Status() == payment.StatusCancelled })
	if got := e.mustPayment("ord-1"); got.Outcome() != payment.OutcomeCancelled || got.TerminalAt().IsZero() {
		t.Fatalf("cancelled payment = %s terminal %v", got.Outcome(), got.TerminalAt())
	}
}

// docs/payment.md §4 亂序與重送: the end of an order can reach payment before its OrderConfirmed (they are
// different topics). Either way the payment ends up ended, never payable.
func TestConsume_OutOfOrder(t *testing.T) {
	for name, tc := range map[string]struct {
		topic string
		msg   func() proto.Message
		want  payment.Outcome
	}{
		"cancelled before confirmed": {topicCancelled, func() proto.Message { return &orderingv1.OrderCancelled{OrderId: "ord-1"} }, payment.OutcomeCancelled},
		"completed before confirmed": {topicCompleted, func() proto.Message { return &orderingv1.OrderCompleted{OrderId: "ord-1"} }, payment.OutcomeCompletedUnpaid},
	} {
		t.Run(name, func(t *testing.T) {
			e := newConsumeEnv(t)
			e.start(nil)

			e.publish(tc.topic, tc.msg())
			e.eventually("the tombstone", func() bool {
				return e.count(`SELECT count(*) FROM payment.order_outcomes WHERE order_id = 'ord-1' AND outcome = ?`, tc.want.String()) == 1
			})
			if _, err := e.payment("ord-1"); !errors.Is(err, payment.ErrNotFound) {
				t.Fatalf("the end of an order created a payment: %v", err)
			}

			e.publish(topicConfirmed, confirmed("ord-1", money(800)))
			e.eventually("the payment", func() bool { _, err := e.payment("ord-1"); return err == nil })
			p := e.mustPayment("ord-1")
			if p.Status() != payment.StatusCancelled || p.Outcome() != tc.want {
				t.Fatalf("payment = %s/%s, want CANCELLED/%s", p.Status(), p.Outcome(), tc.want)
			}
		})
	}
}

// §2: an OrderConfirmed from before the total existed is a domain error: acknowledged and dropped (never
// poison, never retried), no payment row, an alert.
func TestConsume_OrderConfirmedWithoutATotalCreatesNothing(t *testing.T) {
	e := newConsumeEnv(t)
	e.start(nil)

	e.publish(topicConfirmed, confirmed("ord-old", nil))
	e.publish(topicConfirmed, confirmed("ord-marker", money(100))) // behind it on the same topic: once this exists, the old one was handled
	e.eventually("both events to be consumed", func() bool { return e.consumed(topicConfirmed) == 2 })

	if _, err := e.payment("ord-old"); !errors.Is(err, payment.ErrNotFound) {
		t.Fatalf("an order without a total has a payment: %v", err)
	}
	e.mustPayment("ord-marker") // positive control: the consumer was alive and working
	if n := e.count(`SELECT count(*) FROM payment.events_poison`); n != 0 {
		t.Fatalf("%d rows on the poison topic: a domain rejection must be dropped, not parked", n)
	}
}

// capturedPayment seeds a CAPTURED payment for ord-1 with a paid checkout at the env's mock provider.
func (e *consumeEnv) capturedPayment() *payment.Payment {
	e.t.Helper()
	c := context.Background()
	total, err := kernel.NewMoney(1000, "TWD")
	if err != nil {
		e.t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	p, err := payment.New(payment.NewParams{ID: payment.ID(mustID(e.t)), OrderID: "ord-1", CustomerID: owner, Total: total, ExpiresAt: now.Add(time.Hour)}, now)
	if err != nil {
		e.t.Fatal(err)
	}
	co, err := e.prov.CreateCheckout(c, dto.ProviderCheckoutInput{PaymentID: p.ID().String(), OrderID: "ord-1", Amount: dto.MoneyOutput{Amount: 1000, Currency: "TWD"}})
	if err != nil {
		e.t.Fatal(err)
	}
	if err = p.AttachCheckout(e.prov.Name(), co.Ref, co.URL, co.ExpiresAt, now); err != nil {
		e.t.Fatal(err)
	}
	if _, err = e.prov.Pay(co.Ref, total); err != nil {
		e.t.Fatal(err)
	}
	p.Capture(payment.SourceWebhook, total, now)
	if err = e.repo().Save(c, p); err != nil {
		e.t.Fatal(err)
	}

	return e.mustPayment("ord-1")
}

// T10 + T11 end to end: money held for an order that was cancelled goes back. The refund request travels
// the outbox, the provider is asked once (idempotency key = the payment id), PaymentRefunded is published.
func TestConsume_CancelledOrderOfACapturedPaymentIsRefunded(t *testing.T) {
	e := newConsumeEnv(t)
	p := e.capturedPayment()
	e.start(nil)

	e.publish(topicCancelled, &orderingv1.OrderCancelled{OrderId: "ord-1", CustomerId: owner})
	e.eventually("ord-1 to be REFUNDED", func() bool { return e.mustPayment("ord-1").Status() == payment.StatusRefunded })

	if got := e.mustPayment("ord-1"); got.Outcome() != payment.OutcomeCancelled {
		t.Fatalf("outcome = %s", got.Outcome())
	}
	if n := e.prov.Refunds(p.ProviderRef()); n != 1 {
		t.Fatalf("the provider refunded %d times, want 1", n)
	}
	if n := e.count(`SELECT count(*) FROM payment.events_v1_paymentrefunded`); n != 1 {
		t.Fatalf("%d PaymentRefunded events, want 1", n)
	}
	if n := e.count(`SELECT count(*) FROM payment.events_v1_paymentrefundrequested`); n != 1 {
		t.Fatalf("%d PaymentRefundRequested events, want 1", n)
	}
}

// failingRefund is a provider whose Refund fails while down is set and counts every attempt.
type failingRefund struct {
	*mock.Provider
	down  atomic.Bool
	calls atomic.Int64
}

func (f *failingRefund) Refund(c context.Context, in dto.ProviderRefundInput) error {
	f.calls.Add(1)
	if f.down.Load() {
		return errors.New("gateway unreachable")
	}

	return f.Provider.Refund(c, in)
}

// docs/payment.md §3 failure table, "退款失敗": a refund that always fails is retried a bounded number of
// times and then parked on payment.poison while the payment stays REFUNDING; once the provider is back, a
// requeue converges to REFUNDED with one refund at the provider.
func TestConsume_ARefundThatAlwaysFailsIsParkedAndConvergesAfterRecovery(t *testing.T) {
	e := newConsumeEnv(t)
	p := e.capturedPayment()
	prov := &failingRefund{Provider: e.prov}
	prov.down.Store(true)
	e.start(prov)

	e.publish(topicCancelled, &orderingv1.OrderCancelled{OrderId: "ord-1", CustomerId: owner})
	e.eventually("the refund request to be parked on the poison topic", func() bool {
		return e.count(`SELECT count(*) FROM payment.events_poison`) == 1
	})
	if prov.calls.Load() < 2 {
		t.Fatalf("positive control: the provider was asked %d times, want the initial try and the bounded retries", prov.calls.Load())
	}
	if got := e.mustPayment("ord-1").Status(); got != payment.StatusRefunding {
		t.Fatalf("status = %s while the refund fails, want REFUNDING", got)
	}
	if n := e.prov.Refunds(p.ProviderRef()); n != 0 {
		t.Fatalf("the provider recorded %d refunds although every call failed", n)
	}

	prov.down.Store(false)
	n, err := eventbus.Requeue(context.Background(), e.db, eventbus.PoisonTopic("payment"), e.poisonOffsets()...)
	if err != nil || n != 1 {
		t.Fatalf("requeue = %d, %v", n, err)
	}
	e.eventually("ord-1 to be REFUNDED after recovery", func() bool { return e.mustPayment("ord-1").Status() == payment.StatusRefunded })
	if got := e.prov.Refunds(p.ProviderRef()); got != 1 {
		t.Fatalf("refund keys at the provider = %d, want 1", got)
	}
}

func (e *consumeEnv) poisonOffsets() []int64 {
	e.t.Helper()
	var offsets []int64
	if err := e.db.Raw(`SELECT "offset" FROM payment.events_poison ORDER BY "offset"`).Scan(&offsets).Error; err != nil {
		e.t.Fatal(err)
	}

	return offsets
}
