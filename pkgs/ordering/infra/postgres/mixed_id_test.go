package postgres_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// Same property as the memory test, against the real keyset query: old v4 ids
// and new v7 ids interleave, created_at decides the order and id only breaks
// ties. The ids sort the opposite way to created_at, so an `ORDER BY id`
// shortcut would fail here.
func TestOrderRepository_List_MixedV4V7_Postgres(t *testing.T) {
	c := context.Background()
	cfg := config.Postgres{URL: pgtest.URL(t)}
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	db, cleanup, err := platformpg.NewDB(cfg, tel)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := platformpg.Up(c, cfg, postgres.Migration()); err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(c).Exec(`TRUNCATE ordering.orders CASCADE`).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.WithContext(context.Background()).Exec(`TRUNCATE ordering.orders CASCADE`).Error })

	repo := postgres.NewOrderRepository(db)
	base := time.Now().UTC().Truncate(time.Microsecond)
	usd, _ := kernel.NewMoney(100, "USD")
	li, _ := order.NewLineItem("A", 1, usd)

	for _, spec := range []struct {
		id string
		at time.Time
	}{
		{"ffffffff-ffff-4fff-bfff-ffffffffffff", base},
		{"01920000-0000-7000-8000-000000000001", base.Add(time.Second)},
		{"00000000-0000-4000-8000-000000000002", base.Add(2 * time.Second)},
		{"01920000-0000-7000-8000-000000000003", base.Add(2 * time.Second)},
		{"01920000-0000-7000-8000-000000000004", base.Add(3 * time.Second)},
	} {
		o, err := order.New(order.Params{ID: order.ID(spec.id), CustomerID: "c-mixed", Items: []order.LineItem{li}, Now: spec.at})
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.Save(c, o); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{
		"01920000-0000-7000-8000-000000000004",
		"01920000-0000-7000-8000-000000000003",
		"00000000-0000-4000-8000-000000000002",
		"01920000-0000-7000-8000-000000000001",
		"ffffffff-ffff-4fff-bfff-ffffffffffff",
	}
	for _, limit := range []int{1, 2, 3, 10} {
		var got []string
		var after *order.Cursor
		for {
			page, next, err := repo.List(c, order.ListParams{CustomerID: "c-mixed", Limit: limit, After: after})
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
