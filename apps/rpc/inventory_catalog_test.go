package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"gorm.io/gorm"

	catalogv1 "github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1/catalogv1connect"
	catalogpg "github.com/blackhorseya/x-monorepo/pkgs/catalog/infra/postgres"
	inventoryv1 "github.com/blackhorseya/x-monorepo/pkgs/inventory/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/contract/v1/inventoryv1connect"
	inventorypg "github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	orderingpg "github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Terminate()
	if binPath != "" {
		_ = os.RemoveAll(filepath.Dir(binPath)) // the catalog-rpc binary the tests built
	}
	os.Exit(code)
}

// catalogStockServer is the real apps/rpc injector on real Postgres (auth off): catalog,
// inventory and ordering share one database, as in production. The returned db is a second
// handle for what the API cannot do: plant an orphan stock row, break the catalog's table.
type catalogStockServer struct {
	url       string
	inv       inventoryv1connect.InventoryServiceClient
	catalog   catalogv1connect.CatalogServiceClient
	db        *gorm.DB
	stockRows func(t *testing.T, sku string) int64
}

func newCatalogStockServer(t *testing.T) catalogStockServer {
	t.Helper()
	pgCfg := config.Postgres{URL: pgtest.URL(t)}
	c := context.Background()

	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(telCleanup)
	db, dbCleanup, err := postgres.NewDB(pgCfg, tel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dbCleanup)
	for _, m := range []postgres.Migration{orderingpg.Migration(), inventorypg.Migration(), catalogpg.Migration()} {
		if err := postgres.Up(c, pgCfg, m); err != nil {
			t.Fatal(err)
		}
	}
	for _, tbl := range []string{"inventory.stocks", "catalog.products"} {
		if err := db.WithContext(c).Exec("TRUNCATE " + tbl + " CASCADE").Error; err != nil {
			t.Fatal(err)
		}
	}

	// the catalog is its own process on the same database, as in production
	cat := startCatalogProc(t, "APP_POSTGRES_URL="+pgCfg.URL)
	srv, cleanup, err := initialize(t, withCatalog(&config.AppConfig{
		HTTP:     config.HTTP{Addr: ":0"},
		Log:      config.Log{Level: "error", Format: "text"},
		Auth:     config.Auth{Mode: config.AuthOff},
		Postgres: pgCfg,
	}, cat))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return catalogStockServer{
		url:     ts.URL,
		inv:     inventoryv1connect.NewInventoryServiceClient(http.DefaultClient, ts.URL),
		catalog: catalogv1connect.NewCatalogServiceClient(http.DefaultClient, ts.URL),
		db:      db,
		stockRows: func(t *testing.T, sku string) int64 {
			t.Helper()
			var n int64
			if err := db.WithContext(c).Raw(`SELECT count(*) FROM inventory.stocks WHERE sku = ?`, sku).Scan(&n).Error; err != nil {
				t.Fatal(err)
			}

			return n
		},
	}
}

func connectMessage(t *testing.T, err error) string {
	t.Helper()
	var ce *connect.Error
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want a connect error", err)
	}

	return ce.Message()
}

func (s catalogStockServer) setStock(sku string, onHand int32) error {
	_, err := s.inv.SetStock(context.Background(), connect.NewRequest(&inventoryv1.SetStockRequest{Sku: sku, OnHand: onHand}))

	return err
}

func (s catalogStockServer) getStock(sku string) error {
	_, err := s.inv.GetStock(context.Background(), connect.NewRequest(&inventoryv1.GetStockRequest{Sku: sku}))

	return err
}

func (s catalogStockServer) upsert(t *testing.T, sku string) {
	t.Helper()
	seedProduct(t, s.url, "", sku, 100, "USD")
}

// A SKU the catalog does not have cannot be stocked: the refusal is the caller's to see at
// write time, and nothing is written. The product put on sale afterwards makes it work.
func TestInventory_SetStockRefusesASKUTheCatalogLacks(t *testing.T) {
	s := newCatalogStockServer(t)

	err := s.setStock("NO-SUCH-PRODUCT", 5)
	wantConnectCode(t, err, connect.CodeInvalidArgument, "stock.unknown_sku")
	if msg := connectMessage(t, err); msg != "the catalog has no product with this sku" {
		t.Fatalf("message = %q, want the fixed text", msg)
	}
	if n := s.stockRows(t, "NO-SUCH-PRODUCT"); n != 0 {
		t.Fatalf("%d stock rows were written for a SKU the catalog does not have", n)
	}
	wantConnectCode(t, s.getStock("NO-SUCH-PRODUCT"), connect.CodeNotFound, "stock.not_found")
}

// A product taken off sale can still be counted and cleared: SetStock does not look at `active`.
func TestInventory_SetStockAllowsADeactivatedProduct(t *testing.T) {
	s := newCatalogStockServer(t)
	s.upsert(t, "OFF-SALE")
	if err := s.setStock("OFF-SALE", 3); err != nil {
		t.Fatalf("active: %v", err)
	}
	if _, err := s.catalog.DeactivateProduct(context.Background(), connect.NewRequest(&catalogv1.DeactivateProductRequest{Sku: "OFF-SALE"})); err != nil {
		t.Fatal(err)
	}
	if err := s.setStock("OFF-SALE", 0); err != nil {
		t.Fatalf("deactivated: %v", err)
	}
}

// With the catalog failing (its table is gone under it), SetStock fails closed with
// UNAVAILABLE and writes nothing, while the reads of stock keep answering.
func TestInventory_SetStockFailsClosedWhenTheCatalogFails(t *testing.T) {
	s := newCatalogStockServer(t)
	c := context.Background()
	s.upsert(t, "KNOWN")
	if err := s.setStock("KNOWN", 7); err != nil {
		t.Fatal(err)
	}

	if err := s.db.WithContext(c).Exec(`ALTER TABLE catalog.products RENAME TO products_broken`).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.db.Exec(`ALTER TABLE catalog.products_broken RENAME TO products`).Error; err != nil {
			t.Error(err)
		}
	})

	down := s.setStock("KNOWN", 99)
	wantConnectCode(t, down, connect.CodeUnavailable, "catalog.unavailable")
	// the catalog's own error (a missing relation) stays in the log, never in the answer
	if msg := connectMessage(t, down); strings.Contains(msg, "relation") || strings.Contains(msg, "products") || strings.Contains(msg, "SQLSTATE") {
		t.Fatalf("message %q leaks the catalog's error", msg)
	}
	wantConnectCode(t, s.setStock("NEW-SKU", 1), connect.CodeUnavailable, "catalog.unavailable")
	if n := s.stockRows(t, "NEW-SKU"); n != 0 {
		t.Fatalf("%d stock rows were written while the catalog was failing", n)
	}
	got, err := s.inv.GetStock(c, connect.NewRequest(&inventoryv1.GetStockRequest{Sku: "KNOWN"}))
	if err != nil || got.Msg.GetStock().GetOnHand() != 7 {
		t.Fatalf("GetStock while the catalog fails = %v, %v; want the untouched 7", got, err)
	}
	list, err := s.inv.ListStocks(c, connect.NewRequest(&inventoryv1.ListStocksRequest{}))
	if err != nil || len(list.Msg.GetStocks()) != 1 {
		t.Fatalf("ListStocks while the catalog fails = %v, %v", list, err)
	}
}

// Stock that predates this rule (a row with no product) is still readable; setting it needs
// the product first.
func TestInventory_OrphanStockStaysReadableAndNeedsAProductToChange(t *testing.T) {
	s := newCatalogStockServer(t)
	c := context.Background()
	if err := s.db.WithContext(c).Exec(`INSERT INTO inventory.stocks (sku, on_hand, reserved, version, updated_at) VALUES ('ORPHAN', 4, 0, 1, now())`).Error; err != nil {
		t.Fatal(err)
	}

	if err := s.getStock("ORPHAN"); err != nil {
		t.Fatalf("GetStock orphan: %v", err)
	}
	wantConnectCode(t, s.setStock("ORPHAN", 9), connect.CodeInvalidArgument, "stock.unknown_sku")
	s.upsert(t, "ORPHAN")
	if err := s.setStock("ORPHAN", 9); err != nil {
		t.Fatalf("SetStock once the product exists: %v", err)
	}
}
