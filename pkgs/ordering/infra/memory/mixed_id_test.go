package memory_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/memory"
)

// v4 ids (written before the v7 switch) and v7 ids live side by side. The
// listing order is (created_at desc, id desc): created_at decides, id only
// breaks ties. Here the ids deliberately sort the opposite way to created_at
// (a "newer" order has the smaller id), so a listing that leaned on id alone
// would come back wrong.
func TestOrderRepository_List_MixedV4V7(t *testing.T) {
	c := context.Background()
	repo := memory.NewOrderRepository()
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	usd, _ := kernel.NewMoney(100, "USD")
	li, _ := order.NewLineItem("A", 1, usd)

	for _, spec := range []struct {
		id string
		at time.Time
	}{
		{"ffffffff-ffff-4fff-bfff-ffffffffffff", base},                      // v4, oldest, largest id
		{"01920000-0000-7000-8000-000000000001", base.Add(time.Second)},     // v7, tiny id, newer
		{"00000000-0000-4000-8000-000000000002", base.Add(2 * time.Second)}, // v4, tie with the next
		{"01920000-0000-7000-8000-000000000003", base.Add(2 * time.Second)}, // v7, tie with the previous
		{"01920000-0000-7000-8000-000000000004", base.Add(3 * time.Second)}, // v7, newest
	} {
		o, err := order.New(order.Params{ID: order.ID(spec.id), CustomerID: "c-1", Items: []order.LineItem{li}, Now: spec.at})
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.Save(c, o); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{
		"01920000-0000-7000-8000-000000000004",
		"01920000-0000-7000-8000-000000000003", // tie at +2s: larger id first
		"00000000-0000-4000-8000-000000000002",
		"01920000-0000-7000-8000-000000000001",
		"ffffffff-ffff-4fff-bfff-ffffffffffff",
	}
	for _, limit := range []int{1, 2, 3, 10} {
		var got []string
		var after *order.Cursor
		for {
			page, next, err := repo.List(c, order.ListParams{Limit: limit, After: after})
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range page {
				got = append(got, o.ID().String())
			}
			if next == nil {
				break
			}
			after = next
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("limit %d: walk = %v, want %v", limit, got, want)
		}
	}
}
