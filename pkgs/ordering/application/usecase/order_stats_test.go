package usecase_test

import (
	"context"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/ownership"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/memory"
)

// Every known status is reported, zero when empty: a gauge series must not
// vanish when its status drains, or "PENDING too old" alerts see no data.
func TestOrderStats_ZeroFills(t *testing.T) {
	c := context.Background()
	repo := memory.NewOrderRepository()
	t0 := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	rec := &recorder{}
	stats := usecase.NewOrderStats(repo)

	out, err := stats.Execute(c, dto.OrderStatsInput{})
	if err != nil || len(out.Statuses) != 4 {
		t.Fatalf("empty stats = %+v, %v", out, err)
	}
	for _, s := range out.Statuses {
		if s.Count != 0 || !s.OldestUpdatedAt.IsZero() {
			t.Fatalf("empty status %s = %+v", s.Status, s)
		}
	}

	for i, id := range []string{"o-1", "o-2", "o-3"} {
		create := usecase.NewCreateOrder(repo, rec, fixedIDs{id: id}, fixedClock{t: t0.Add(time.Duration(i) * time.Minute)}, ownership.New("admin"))
		if _, err := create.Execute(c, dto.CreateOrderInput{CustomerID: "c-1", Items: []dto.LineItem{{SKU: "S", Quantity: 1, UnitPrice: dto.Money{AmountMinor: 1, Currency: "USD"}}}}); err != nil {
			t.Fatal(err)
		}
	}
	cancel := usecase.NewCancelOrder(repo, rec, fixedClock{t: t0.Add(time.Hour)}, ownership.New("admin"))
	if _, err := cancel.Execute(c, dto.CancelOrderInput{OrderID: "o-3"}); err != nil {
		t.Fatal(err)
	}

	out, err = stats.Execute(c, dto.OrderStatsInput{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]dto.StatusStat{
		"PENDING":   {Status: "PENDING", Count: 2, OldestUpdatedAt: t0},
		"CONFIRMED": {Status: "CONFIRMED"},
		"COMPLETED": {Status: "COMPLETED"},
		"CANCELLED": {Status: "CANCELLED", Count: 1, OldestUpdatedAt: t0.Add(time.Hour)},
	}
	if len(out.Statuses) != len(want) {
		t.Fatalf("stats = %+v", out.Statuses)
	}
	for _, s := range out.Statuses {
		w := want[s.Status]
		if s.Count != w.Count || !s.OldestUpdatedAt.Equal(w.OldestUpdatedAt) {
			t.Fatalf("%s = %+v, want %+v", s.Status, s, w)
		}
	}
}
