package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/ownership"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

type fixedClock struct{}

func (fixedClock) Now() time.Time { return time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC) }

type sink struct {
	mu     sync.Mutex
	events []string
}

func (s *sink) Publish(_ context.Context, evs ...order.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range evs {
		s.events = append(s.events, fmt.Sprintf("%T", e))
	}

	return nil
}

func (s *sink) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string{}, s.events...)
}

// gated is a repository whose first Save of the loser waits until the winner has finished: both have
// loaded the CONFIRMED order and decided, then the winner saves, then the loser's compare-and-set
// fails and it must reload and decide again on what the winner left (docs/payment.md §5/§8: "付款事件
// 與取消併發"). entered tells the test the loser reached its Save.
type gated struct {
	order.Repository
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gated) Save(c context.Context, o *order.Order) error {
	g.once.Do(func() {
		close(g.entered)
		<-g.release
	})

	return g.Repository.Save(c, o)
}

// The three things that can happen to a CONFIRMED order that requires payment — the customer cancels,
// payment says captured, payment says expired — raced pairwise on real Postgres in both finishing
// orders. Exactly one wins; the loser, reloading, is refused by the state machine; a CANCELLED order is
// never revived and a COMPLETED one is never cancelled. The default-snapshot (false) row is the control:
// there the payment events change nothing, whoever runs.
func TestPaymentEvents_RacesOnPostgres(t *testing.T) {
	url := pgtest.URL(t)
	c := context.Background()
	cfg := config.Postgres{URL: url}
	db := pgtest.DB(t, cfg, pgtest.Telemetry(t))
	if err := platformpg.Up(c, cfg, postgres.Migration()); err != nil {
		t.Fatal(err)
	}
	real := postgres.NewOrderRepository(db)
	own := ownership.New("admin")

	type actor struct {
		name string
		run  func(repo order.Repository, ev *sink) error
		// final is the status the order ends in when this actor wins; refusal is what it gets when it loses.
		final   string
		refusal error
	}
	captured := actor{"captured", func(r order.Repository, ev *sink) error {
		_, err := usecase.NewApplyPaymentCaptured(r, ev, fixedClock{}).Execute(c, dto.PaymentCapturedInput{OrderID: "race-1"})
		return err
	}, "COMPLETED", order.ErrCannotPay}
	expired := actor{"expired", func(r order.Repository, ev *sink) error {
		_, err := usecase.NewApplyPaymentExpired(r, ev, fixedClock{}).Execute(c, dto.PaymentExpiredInput{OrderID: "race-1"})
		return err
	}, "CANCELLED", order.ErrCannotCancel}
	cancel := actor{"cancel", func(r order.Repository, ev *sink) error {
		_, err := usecase.NewCancelOrder(r, ev, fixedClock{}, own).Execute(c, dto.CancelOrderInput{OrderID: "race-1", Source: dto.CancelSourceRPC})
		return err
	}, "CANCELLED", order.ErrCannotCancel}

	seed := func(t *testing.T) {
		t.Helper()
		if err := db.WithContext(c).Exec(`TRUNCATE ordering.orders CASCADE`).Error; err != nil {
			t.Fatal(err)
		}
		usd, _ := kernel.NewMoney(500, "USD")
		it, err := order.NewLineItem("S", 1, usd)
		if err != nil {
			t.Fatal(err)
		}
		o, err := order.Reconstitute(order.ReconstituteParams{
			ID: "race-1", CustomerID: "c-1", Items: []order.LineItem{it}, Status: order.StatusConfirmed, PaymentRequired: true,
			CreatedAt: fixedClock{}.Now(), UpdatedAt: fixedClock{}.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err = real.Save(c, o); err != nil {
			t.Fatal(err)
		}
	}
	status := func(t *testing.T) string {
		t.Helper()
		o, err := real.FindByID(c, "race-1")
		if err != nil {
			t.Fatal(err)
		}

		return o.Status().String()
	}

	for _, pair := range [][2]actor{{captured, cancel}, {captured, expired}, {expired, cancel}} {
		for _, flip := range []bool{false, true} {
			winner, loser := pair[0], pair[1]
			if flip {
				winner, loser = loser, winner
			}
			t.Run(fmt.Sprintf("%s wins, %s loses", winner.name, loser.name), func(t *testing.T) {
				seed(t)
				g := &gated{Repository: real, entered: make(chan struct{}), release: make(chan struct{})}
				loserEvents, winnerEvents := &sink{}, &sink{}

				loserDone := make(chan error, 1)
				go func() { loserDone <- loser.run(g, loserEvents) }() // fire-and-forget: joined through loserDone below
				select {
				case <-g.entered:
				case <-time.After(3 * time.Second):
					t.Fatal("the loser never reached its Save")
				}
				if err := winner.run(real, winnerEvents); err != nil {
					t.Fatalf("winner %s: %v", winner.name, err)
				}
				close(g.release)

				var err error
				select {
				case err = <-loserDone:
				case <-time.After(3 * time.Second):
					t.Fatal("the loser never finished")
				}

				got := status(t)
				if got != winner.final {
					t.Errorf("final status = %s, want %s (the winner's)", got, winner.final)
				}
				if winner.final == loser.final {
					// the same end reached by two roads (cancel vs expired): the loser finds it done
					if loser.name == "cancel" && !errors.Is(err, order.ErrCannotCancel) {
						t.Errorf("cancel after the order was cancelled: err = %v, want order.cannot_cancel", err)
					}
					if loser.name == "expired" && err != nil {
						t.Errorf("expired after the order was cancelled is a no-op, got %v", err)
					}
				} else if !errors.Is(err, loser.refusal) {
					t.Errorf("loser %s err = %v, want %v", loser.name, err, loser.refusal)
				}
				if ev := loserEvents.names(); len(ev) != 0 {
					t.Errorf("the loser raised %v: a refused or no-op event must raise nothing", ev)
				}
			})
		}
	}
}
