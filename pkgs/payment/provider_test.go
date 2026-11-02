package payment_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	paymentbc "github.com/blackhorseya/x-monorepo/pkgs/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

var t0 = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)

type fixedClock struct{}

func (fixedClock) Now() time.Time { return t0 }

type nopEvents struct{}

func (nopEvents) Publish(context.Context, *payment.Payment, []payment.Event, time.Time) error {
	return nil
}
func (nopEvents) PublishWebhookReceived(context.Context, string, string, time.Time) error { return nil }

// setInbox is an in-memory consumer inbox: the first Record of an id is first.
type setInbox struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (s *setInbox) Record(_ context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	first := !s.seen[id]
	s.seen[id] = true

	return first, nil
}

type chainEnv struct {
	repo  *memory.PaymentRepository
	prov  *mock.Provider
	logs  *bytes.Buffer
	uc    usecasex.UseCase[dto.OrderEndedInput, *dto.OrderEndedOutput]
	asked func(string) int
}

// newChain builds the ProvideRecordOrderCancelled chain of provider.go over in-memory parts, with a
// payment of order "ord-1" that has a checkout open at the mock provider.
func newChain(t *testing.T, fault func(op string) error) *chainEnv {
	t.Helper()
	var mu sync.Mutex
	asked := map[string]int{}
	prov := mock.New(mock.Options{Now: fixedClock{}.Now, Fault: func(op string) error {
		mu.Lock()
		asked[op]++
		mu.Unlock()
		if fault != nil {
			return fault(op)
		}

		return nil
	}})
	repo := memory.NewPaymentRepository()
	total, err := kernel.NewMoney(1000, "TWD")
	if err != nil {
		t.Fatal(err)
	}
	p, err := payment.New(payment.NewParams{ID: "pay-1", OrderID: "ord-1", CustomerID: "cust", Total: total, ExpiresAt: t0.Add(30 * time.Minute)}, t0)
	if err != nil {
		t.Fatal(err)
	}
	co, err := prov.CreateCheckout(context.Background(), dto.ProviderCheckoutInput{PaymentID: "pay-1", OrderID: "ord-1", Amount: dto.MoneyOutput{Amount: 1000, Currency: "TWD"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = p.AttachCheckout(prov.Name(), co.Ref, co.URL, co.ExpiresAt, t0); err != nil {
		t.Fatal(err)
	}
	if err = repo.Save(context.Background(), p); err != nil {
		t.Fatal(err)
	}

	logs := &bytes.Buffer{}
	rec, err := usecasex.NewRecorder(noop.NewMeterProvider())
	if err != nil {
		t.Fatal(err)
	}
	uc := paymentbc.ProvideRecordOrderCancelled(repo, memory.NewOutcomeRepository(), nopEvents{}, fixedClock{}, prov,
		slog.New(slog.NewTextHandler(logs, nil)), memory.NewTransactor(), &setInbox{}, rec)

	return &chainEnv{repo: repo, prov: prov, logs: logs, uc: usecasex.UseCase[dto.OrderEndedInput, *dto.OrderEndedOutput](uc), asked: func(op string) int {
		mu.Lock()
		defer mu.Unlock()

		return asked[op]
	}}
}

func msgCtx(id string) context.Context { return contextx.WithMessageID(context.Background(), id) }

// T7: the checkout the payment had open is cancelled at the provider once the order's cancellation was
// recorded.
func TestEndedChain_CancelsTheOpenCheckoutAfterTheCommit(t *testing.T) {
	e := newChain(t, nil)
	if _, err := e.uc.Execute(msgCtx("m-1"), dto.OrderEndedInput{OrderID: "ord-1"}); err != nil {
		t.Fatal(err)
	}
	p, _ := e.repo.FindByOrderID(context.Background(), "ord-1")
	if p.Status() != payment.StatusCancelled {
		t.Fatalf("status = %s", p.Status())
	}
	if !e.prov.Cancelled(p.ProviderRef()) || e.asked(mock.OpCancelCheckout) != 1 {
		t.Fatalf("checkout cancelled = %v after %d calls, want true after 1", e.prov.Cancelled(p.ProviderRef()), e.asked(mock.OpCancelCheckout))
	}
}

// docs/payment.md §3 failure table, "取消 checkout 失敗": the payment goes CANCELLED anyway and the failure
// is a fixed category in the log (never the provider's words); the reconcile sweep owns what that leaves.
func TestEndedChain_ACancelThatAlwaysFailsBlocksNothing(t *testing.T) {
	const marker = "SECRET-CANCEL-MARKER-55e1"
	e := newChain(t, func(op string) error {
		if op == mock.OpCancelCheckout {
			return errors.New("gateway says " + marker)
		}

		return nil
	})
	if _, err := e.uc.Execute(msgCtx("m-1"), dto.OrderEndedInput{OrderID: "ord-1"}); err != nil {
		t.Fatalf("a failing cancel blocked the transition: %v", err)
	}
	p, _ := e.repo.FindByOrderID(context.Background(), "ord-1")
	if p.Status() != payment.StatusCancelled || p.Outcome() != payment.OutcomeCancelled {
		t.Fatalf("payment = %s/%s", p.Status(), p.Outcome())
	}
	if e.asked(mock.OpCancelCheckout) != 1 {
		t.Fatalf("positive control: the provider was asked to cancel %d times, want 1", e.asked(mock.OpCancelCheckout))
	}
	if e.prov.Cancelled(p.ProviderRef()) {
		t.Fatal("the fake says the checkout was cancelled although every cancel failed")
	}
	logs := e.logs.String()
	if !strings.Contains(logs, "payment.cancel_checkout_failed") {
		t.Fatalf("the failure left no fixed-category line: %q", logs)
	}
	if strings.Contains(logs, marker) {
		t.Fatalf("the provider's words reached the log: %q", logs)
	}
}

// A redelivered message is dropped by the inbox before anything runs: the checkout is not cancelled a
// second time and the payment is not touched.
func TestEndedChain_ARedeliveryDoesNothing(t *testing.T) {
	e := newChain(t, nil)
	if _, err := e.uc.Execute(msgCtx("m-1"), dto.OrderEndedInput{OrderID: "ord-1"}); err != nil {
		t.Fatal(err)
	}
	before, _ := e.repo.FindByOrderID(context.Background(), "ord-1")
	// the inbox answers message.duplicate (a domain error: the consumer drops it, ack and done)
	var de *kernel.Error
	if _, err := e.uc.Execute(msgCtx("m-1"), dto.OrderEndedInput{OrderID: "ord-1"}); !errors.As(err, &de) || de.Code() != "message.duplicate" {
		t.Fatalf("redelivery err = %v, want message.duplicate", err)
	}
	after, _ := e.repo.FindByOrderID(context.Background(), "ord-1")
	if after.Version() != before.Version() || e.asked(mock.OpCancelCheckout) != 1 {
		t.Fatalf("redelivery: version %d -> %d, cancel calls %d; want unchanged and 1", before.Version(), after.Version(), e.asked(mock.OpCancelCheckout))
	}
}
