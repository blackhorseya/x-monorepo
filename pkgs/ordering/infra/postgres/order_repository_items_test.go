package postgres_test

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// itemTuple is one order_items row with its physical address. A row that was
// deleted and re-inserted keeps every column but gets a new ctid.
type itemTuple struct {
	CTID     string `gorm:"column:ctid"`
	Position int
	SKU      string
	Quantity int32
}

func itemTuples(t *testing.T, db *gorm.DB, orderID string) []itemTuple {
	t.Helper()
	var rows []itemTuple
	err := db.WithContext(context.Background()).Raw(
		`SELECT ctid::text AS ctid, position, sku, quantity FROM ordering.order_items WHERE order_id = ? ORDER BY position`, orderID,
	).Scan(&rows).Error
	if err != nil {
		t.Fatal(err)
	}

	return rows
}

// A status transition changes only the orders row. Items are fixed once the
// order exists, so Save must not rewrite them: the ctid of every order_items row
// stays the same across Confirm, Complete and Cancel.
func TestOrderRepository_StatusSaveLeavesItemsUntouched(t *testing.T) {
	url := pgtest.URL(t)

	c := context.Background()
	cfg := config.Postgres{URL: url}
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(telCleanup)
	db, cleanup, err := platformpg.NewDB(cfg, tel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup) // registered before purge below: Cleanup is LIFO, so the purge runs while the pool is open
	if err := platformpg.Up(c, cfg, postgres.Migration()); err != nil {
		t.Fatal(err)
	}

	// Own fixture: purge this test's orders (order_items cascade) before and after,
	// so a repeated run (-count=N) starts clean and nothing is left for other tests.
	purge := func() {
		if err := db.WithContext(c).Exec(`DELETE FROM ordering.orders WHERE id LIKE 'items-untouched-%'`).Error; err != nil {
			t.Errorf("purge items-untouched orders: %v", err)
		}
	}
	purge()
	t.Cleanup(purge)

	repo := postgres.NewOrderRepository(db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	usd, err := kernel.NewMoney(100, "USD")
	if err != nil {
		t.Fatal(err)
	}
	li := func(sku string, qty int32) order.LineItem {
		it, err := order.NewLineItem(sku, qty, usd)
		if err != nil {
			t.Fatal(err)
		}
		return it
	}

	for _, tc := range []struct {
		name string
		path []func(*order.Order, time.Time) error
	}{
		{"confirm then complete", []func(*order.Order, time.Time) error{(*order.Order).Confirm, (*order.Order).Complete}},
		{"confirm then cancel", []func(*order.Order, time.Time) error{(*order.Order).Confirm, (*order.Order).Cancel}},
		{"cancel", []func(*order.Order, time.Time) error{(*order.Order).Cancel}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := "items-untouched-" + tc.name
			o, err := order.New(order.Params{ID: order.ID(id), CustomerID: "c-items", Items: []order.LineItem{li("B", 2), li("A", 1), li("C", 3)}, Now: now})
			if err != nil {
				t.Fatal(err)
			}
			if err := repo.Save(c, o); err != nil {
				t.Fatal(err)
			}

			before := itemTuples(t, db, id)
			if len(before) != 3 { // an empty-vs-empty comparison would pass whatever Save does
				t.Fatalf("order_items rows before = %d, want 3", len(before))
			}

			for n, step := range tc.path {
				cur, err := repo.FindByID(c, order.ID(id))
				if err != nil {
					t.Fatal(err)
				}
				if err := step(cur, now.Add(time.Duration(n+1)*time.Minute)); err != nil {
					t.Fatal(err)
				}
				if err := repo.Save(c, cur); err != nil {
					t.Fatal(err)
				}

				after := itemTuples(t, db, id)
				if len(after) != len(before) {
					t.Fatalf("step %d: order_items rows = %d, want %d", n, len(after), len(before))
				}
				for k := range before {
					if after[k] != before[k] {
						t.Fatalf("step %d: order_items row %d = %+v, want the untouched %+v (rewritten by Save)", n, k, after[k], before[k])
					}
				}
			}

			final, err := repo.FindByID(c, order.ID(id))
			if err != nil {
				t.Fatal(err)
			}
			if got := final.Items(); len(got) != 3 || got[0].SKU() != "B" || got[1].SKU() != "A" || got[2].SKU() != "C" {
				t.Fatalf("items after transitions = %v, want B A C in placed order", got)
			}
		})
	}
}
