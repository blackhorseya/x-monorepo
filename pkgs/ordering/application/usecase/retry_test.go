package usecase_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/ownership"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/memory"
)

// racyRepo wraps a real repository and lets a test play "the other writer":
// beforeSave runs just before the n-th Save reaches the inner repository, so a
// competing change lands first and the real optimistic lock rejects the Save.
type racyRepo struct {
	order.Repository
	saves      int
	beforeSave func(n int)
	failWith   error // when set, every Save returns it without touching the inner repo
}

func (r *racyRepo) Save(c context.Context, o *order.Order) error {
	r.saves++
	if r.failWith != nil {
		return r.failWith
	}
	if r.beforeSave != nil {
		r.beforeSave(r.saves)
	}

	return r.Repository.Save(c, o)
}

func seedOrder(t *testing.T, inner order.Repository, clock fixedClock) {
	t.Helper()

	create := usecase.NewCreateOrder(inner, memory.NewTransactor(), &recorder{}, fixedIDs{id: "o-1"}, clock, ownership.New("admin"), memory.NewFlatPriceList(500, "USD"))
	if _, err := create.Execute(context.Background(), dto.CreateOrderInput{
		CustomerID: "c-1",
		Items:      []dto.OrderLine{{SKU: "SKU-1", Quantity: 1}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// Confirm loses to a cancel that commits between its load and its save: the
// retry reloads CANCELLED and the domain refuses, instead of surfacing ErrStale.
func TestConfirmRetriesStaleAndSeesCancel(t *testing.T) {
	c := context.Background()
	clock := fixedClock{t: time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)}
	inner := memory.NewOrderRepository()
	seedOrder(t, inner, clock)

	rec := &recorder{}
	repo := &racyRepo{Repository: inner}
	repo.beforeSave = func(n int) {
		if n != 1 {
			return
		}
		if _, err := usecase.NewCancelOrder(inner, &recorder{}, clock, ownership.New("admin")).Execute(c, dto.CancelOrderInput{OrderID: "o-1"}); err != nil {
			t.Fatalf("competing cancel: %v", err)
		}
	}

	_, err := usecase.NewConfirmOrder(repo, rec, clock).Execute(c, dto.ConfirmOrderInput{OrderID: "o-1"})
	if !errors.Is(err, order.ErrCannotConfirm) {
		t.Fatalf("confirm err = %v, want ErrCannotConfirm (domain verdict after reload)", err)
	}
	if len(rec.events) != 0 {
		t.Fatalf("published %d events for a refused confirm, want 0", len(rec.events))
	}
}

// Cancel loses to a confirm: CONFIRMED is still cancellable, so the retry
// succeeds and the caller never sees the conflict.
func TestCancelRetriesStaleAfterConfirm(t *testing.T) {
	c := context.Background()
	clock := fixedClock{t: time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)}
	inner := memory.NewOrderRepository()
	seedOrder(t, inner, clock)

	rec := &recorder{}
	repo := &racyRepo{Repository: inner}
	repo.beforeSave = func(n int) {
		if n != 1 {
			return
		}
		if _, err := usecase.NewConfirmOrder(inner, &recorder{}, clock).Execute(c, dto.ConfirmOrderInput{OrderID: "o-1"}); err != nil {
			t.Fatalf("competing confirm: %v", err)
		}
	}

	out, err := usecase.NewCancelOrder(repo, rec, clock, ownership.New("admin")).Execute(c, dto.CancelOrderInput{OrderID: "o-1"})
	if err != nil || out.Status != "CANCELLED" {
		t.Fatalf("cancel = %+v, %v; want CANCELLED after one retry", out, err)
	}
	if repo.saves != 2 {
		t.Fatalf("saves = %d, want 2", repo.saves)
	}
	if len(rec.events) != 1 {
		t.Fatalf("published %d events, want exactly 1 (Cancelled, not once per attempt)", len(rec.events))
	}
}

// Complete loses to a cancel: the retry reloads CANCELLED → ErrCannotComplete.
func TestCompleteRetriesStaleAndSeesCancel(t *testing.T) {
	c := context.Background()
	clock := fixedClock{t: time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)}
	inner := memory.NewOrderRepository()
	seedOrder(t, inner, clock)
	if _, err := usecase.NewConfirmOrder(inner, &recorder{}, clock).Execute(c, dto.ConfirmOrderInput{OrderID: "o-1"}); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	repo := &racyRepo{Repository: inner}
	repo.beforeSave = func(n int) {
		if n != 1 {
			return
		}
		if _, err := usecase.NewCancelOrder(inner, &recorder{}, clock, ownership.New("admin")).Execute(c, dto.CancelOrderInput{OrderID: "o-1"}); err != nil {
			t.Fatalf("competing cancel: %v", err)
		}
	}

	_, err := usecase.NewCompleteOrder(repo, &recorder{}, clock, ownership.New("admin")).Execute(c, dto.CompleteOrderInput{OrderID: "o-1"})
	if !errors.Is(err, order.ErrCannotComplete) {
		t.Fatalf("complete err = %v, want ErrCannotComplete", err)
	}
}

// The retry budget is bounded: three attempts, then the original conflict.
func TestRetryExhaustedReturnsStale(t *testing.T) {
	c := context.Background()
	clock := fixedClock{t: time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)}
	inner := memory.NewOrderRepository()
	seedOrder(t, inner, clock)

	repo := &racyRepo{Repository: inner, failWith: order.ErrStale}
	_, err := usecase.NewCancelOrder(repo, &recorder{}, clock, ownership.New("admin")).Execute(c, dto.CancelOrderInput{OrderID: "o-1"})
	if !errors.Is(err, order.ErrStale) {
		t.Fatalf("err = %v, want ErrStale once retries run out", err)
	}
	if repo.saves != 3 {
		t.Fatalf("saves = %d, want 3 attempts", repo.saves)
	}
}

// Only optimistic-lock conflicts are retried; anything else surfaces at once.
func TestRetryIgnoresOtherErrors(t *testing.T) {
	c := context.Background()
	clock := fixedClock{t: time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)}
	inner := memory.NewOrderRepository()
	seedOrder(t, inner, clock)

	for _, failure := range []error{order.ErrDuplicate, errors.New("connection reset")} {
		repo := &racyRepo{Repository: inner, failWith: failure}
		_, err := usecase.NewConfirmOrder(repo, &recorder{}, clock).Execute(c, dto.ConfirmOrderInput{OrderID: "o-1"})
		if !errors.Is(err, failure) {
			t.Fatalf("err = %v, want %v", err, failure)
		}
		if repo.saves != 1 {
			t.Fatalf("%v: saves = %d, want 1 (no retry)", failure, repo.saves)
		}
	}
}

// A cancelled context stops the retry loop instead of sleeping through it.
func TestRetryStopsOnCancelledContext(t *testing.T) {
	clock := fixedClock{t: time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)}
	inner := memory.NewOrderRepository()
	seedOrder(t, inner, clock)

	c, cancel := context.WithCancel(context.Background())
	repo := &racyRepo{Repository: inner, failWith: order.ErrStale}
	repo.beforeSave = nil
	cancelOnFirst := &cancelAfterSave{racyRepo: repo, cancel: cancel}

	_, err := usecase.NewCancelOrder(cancelOnFirst, &recorder{}, clock, ownership.New("admin")).Execute(c, dto.CancelOrderInput{OrderID: "o-1"})
	if err == nil {
		t.Fatal("want an error")
	}
	if repo.saves != 1 {
		t.Fatalf("saves = %d, want 1 (context cancelled after first attempt)", repo.saves)
	}
}

type cancelAfterSave struct {
	*racyRepo
	cancel context.CancelFunc
}

func (r *cancelAfterSave) Save(c context.Context, o *order.Order) error {
	err := r.racyRepo.Save(c, o)
	r.cancel()

	return err
}
