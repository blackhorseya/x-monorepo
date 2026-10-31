package inventory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/stock"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/system"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// Stock that has no catalog product (kept from before SetStock followed the catalog) keeps its
// behavior on every path but SetStock: it is read, reserved, released, committed and expired as
// before, on real Postgres. Only SetStock refuses it, with stock.unknown_sku, until the product
// exists. The catalog here has no product at all.
func TestOrphanStock_KeepsItsBehaviorExceptSetStock(t *testing.T) {
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
	for _, q := range []string{
		`DELETE FROM inventory.reservations WHERE order_id LIKE 'o-orph-%'`,
		`DELETE FROM inventory.stocks WHERE sku LIKE 'ORPH-%'`,
		`INSERT INTO inventory.stocks (sku, on_hand, reserved, version, updated_at) VALUES ('ORPH-1', 10, 0, 1, now())`,
	} {
		if err := db.WithContext(c).Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}

	rec, err := usecasex.NewRecorder(tel.Meter)
	if err != nil {
		t.Fatal(err)
	}
	stocks := inventory.ProvideStockRepository(db, tel.Logger)
	reservations := inventory.ProvideReservationRepository(db)
	tx := inventory.ProvideTransactor(db)
	inbox := inventory.ProvideInbox(db)
	clock := system.NewClock()
	noProducts := memory.NewProductCatalog()
	setStock := inventory.ProvideSetStock(stocks, tx, inbox, clock, noProducts, rec)
	getStock := inventory.ProvideGetStock(stocks, rec)
	listStocks := inventory.ProvideListStocks(stocks, rec)
	reserve := inventory.ProvideReserveStock(reservations, stocks, tx, inbox, publisher{}, clock, rec)
	release := inventory.ProvideReleaseStock(reservations, stocks, tx, inbox, publisher{}, clock, rec)
	commit := inventory.ProvideCommitStock(reservations, stocks, tx, inbox, publisher{}, clock, rec)
	expire := inventory.ProvideExpireReservations(reservations, stocks, tx, inbox, publisher{}, clock, rec)

	want := func(step string, onHand, reserved int32) {
		t.Helper()
		got, err := getStock.Execute(c, dto.GetStockInput{SKU: "ORPH-1"})
		if err != nil || got.OnHand != onHand || got.Reserved != reserved {
			t.Fatalf("%s: stock = %+v, %v; want on_hand %d reserved %d", step, got, err, onHand, reserved)
		}
	}
	hold := func(orderID string, qty int32) {
		t.Helper()
		out, err := reserve.Execute(c, dto.ReserveStockInput{OrderID: orderID, Lines: []dto.Line{{SKU: "ORPH-1", Quantity: qty}}})
		if err != nil || out.Status != "RESERVED" {
			t.Fatalf("reserve %s = %+v, %v; want RESERVED", orderID, out, err)
		}
	}

	want("read", 10, 0)
	list, err := listStocks.Execute(c, dto.ListStocksInput{SKUPrefix: "ORPH-"})
	if err != nil || len(list.Stocks) != 1 || list.Stocks[0].SKU != "ORPH-1" {
		t.Fatalf("ListStocks = %+v, %v", list, err)
	}

	hold("o-orph-release", 2)
	want("reserved", 10, 2)
	if out, err := release.Execute(c, dto.ReleaseStockInput{OrderID: "o-orph-release"}); err != nil || out.Status != "RELEASED" {
		t.Fatalf("release = %+v, %v", out, err)
	}
	want("released", 10, 0)

	hold("o-orph-commit", 3)
	if out, err := commit.Execute(c, dto.CommitStockInput{OrderID: "o-orph-commit"}); err != nil || out.Status != "COMMITTED" {
		t.Fatalf("commit = %+v, %v", out, err)
	}
	want("committed", 7, 0)

	hold("o-orph-expire", 1)
	if err := db.WithContext(c).Exec(`UPDATE inventory.reservations SET updated_at = now() - interval '48 hours' WHERE order_id = 'o-orph-expire'`).Error; err != nil {
		t.Fatal(err)
	}
	if out, err := expire.Execute(c, dto.ExpireReservationsInput{OlderThan: 24 * time.Hour}); err != nil || len(out.Expired) != 1 || out.Expired[0].OrderID != "o-orph-expire" {
		t.Fatalf("expire = %+v, %v; want only o-orph-expire", out, err)
	}
	want("expired", 7, 0)

	// The only path that changed.
	if _, err := setStock.Execute(c, dto.SetStockInput{SKU: "ORPH-1", OnHand: 99}); !errors.Is(err, stock.ErrUnknownSKU) {
		t.Fatalf("SetStock on an orphan = %v, want ErrUnknownSKU", err)
	}
	want("after the refused SetStock", 7, 0)
	if _, err := setStock.Execute(c, dto.SetStockInput{SKU: "ORPH-1", OnHand: 99}); err == nil || noProducts.Calls() != 2 {
		t.Fatalf("SetStock asked the catalog %d times, want 2 (once per call)", noProducts.Calls())
	}
	noProducts.Add("ORPH-1")
	if out, err := setStock.Execute(c, dto.SetStockInput{SKU: "ORPH-1", OnHand: 99}); err != nil || out.OnHand != 99 {
		t.Fatalf("SetStock once the product exists = %+v, %v", out, err)
	}
}
