package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/memory"
)

// The memory repository backs `task run` and the rpc E2E, so it must enforce
// the same Save contract as Postgres or nothing local catches a regression.
func TestOrderRepository_Versioning(t *testing.T) {
	c := context.Background()
	repo := memory.NewOrderRepository()
	now := time.Now()
	usd, _ := kernel.NewMoney(100, "USD")
	item, _ := order.NewLineItem("A", 1, usd)
	fresh := func() *order.Order {
		o, err := order.New(order.Params{ID: "o-1", CustomerID: "c-1", Items: []order.LineItem{item}, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		return o
	}

	if err := repo.Save(c, fresh()); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := repo.Save(c, fresh()); !errors.Is(err, order.ErrDuplicate) {
		t.Fatalf("fresh over existing: err = %v, want ErrDuplicate", err)
	}

	a, _ := repo.FindByID(c, "o-1")
	b, _ := repo.FindByID(c, "o-1")
	if a.Version() != 1 {
		t.Fatalf("loaded version = %d, want 1", a.Version())
	}
	_ = a.Cancel(now)
	_ = b.Cancel(now)
	if err := repo.Save(c, a); err != nil {
		t.Fatalf("first writer: %v", err)
	}
	if err := repo.Save(c, b); !errors.Is(err, order.ErrStale) {
		t.Fatalf("second writer: err = %v, want ErrStale", err)
	}

	got, _ := repo.FindByID(c, "o-1")
	if got.Version() != 2 || got.Status() != order.StatusCancelled {
		t.Fatalf("stored: version=%d status=%v, want 2 CANCELLED", got.Version(), got.Status())
	}

	ghost, _ := order.Reconstitute(order.ReconstituteParams{ID: "o-2", CustomerID: "c-1", Items: []order.LineItem{item}, Status: order.StatusPending, Version: 1})
	if err := repo.Save(c, ghost); !errors.Is(err, order.ErrStale) {
		t.Fatalf("update of a missing order: err = %v, want ErrStale", err)
	}
}

// CountByStatus reports only the statuses that have orders, with the oldest updated_at of each.
func TestOrderRepository_CountByStatus(t *testing.T) {
	c := context.Background()
	repo := memory.NewOrderRepository()
	usd, _ := kernel.NewMoney(100, "USD")
	item, _ := order.NewLineItem("A", 1, usd)
	t0 := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	if got, err := repo.CountByStatus(c); err != nil || len(got) != 0 {
		t.Fatalf("empty = %v, %v", got, err)
	}
	for i, id := range []order.ID{"o-1", "o-2", "o-3"} {
		o, _ := order.New(order.Params{ID: id, CustomerID: "c-1", Items: []order.LineItem{item}, Now: t0.Add(time.Duration(i) * time.Minute)})
		if id == "o-3" {
			_ = o.Cancel(t0.Add(time.Hour))
		}
		if err := repo.Save(c, o); err != nil {
			t.Fatal(err)
		}
	}
	got, err := repo.CountByStatus(c)
	if err != nil || len(got) != 2 {
		t.Fatalf("counts = %v, %v", got, err)
	}
	if got[0].Status() != order.StatusPending || got[0].Count() != 2 || !got[0].OldestUpdatedAt().Equal(t0) {
		t.Fatalf("pending = %+v", got[0])
	}
	if got[1].Status() != order.StatusCancelled || got[1].Count() != 1 || !got[1].OldestUpdatedAt().Equal(t0.Add(time.Hour)) {
		t.Fatalf("cancelled = %+v", got[1])
	}
}
