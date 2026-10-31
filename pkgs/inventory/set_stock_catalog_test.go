package inventory

import (
	"context"
	"errors"
	"sync"
	"testing"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/usecase"
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

type inTxKey struct{}

// spyTransactor wraps the real one: it tags the context fn receives and notes when a
// transaction opens, so a test can tell whether the catalog was asked inside one.
type spyTransactor struct {
	port.Transactor
	log *eventLog
}

func (s spyTransactor) Do(c context.Context, fn func(c context.Context) error) error {
	s.log.add("tx")

	return s.Transactor.Do(c, func(c context.Context) error {
		return fn(context.WithValue(c, inTxKey{}, true))
	})
}

// spyCatalog answers from inner and runs probe at the moment the catalog is asked.
type spyCatalog struct {
	inner port.ProductCatalog
	log   *eventLog
	probe func(c context.Context)
}

func (s spyCatalog) HasProduct(c context.Context, sku string) (bool, error) {
	s.log.add("ask")
	if s.probe != nil {
		s.probe(c)
	}

	return s.inner.HasProduct(c, sku)
}

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]string(nil), l.events...)
}

// observation is what the probe saw at the moment the catalog was asked.
type observation struct {
	inTx   bool // the context the catalog got belongs to an open transaction
	inUse  int  // connections of the fixture's pool checked out
	openTx int  // backends sitting in an open transaction
}

type setStockFixture struct {
	db      *gorm.DB
	stocks  stock.Repository
	tx      port.Transactor
	inbox   port.Inbox
	rec     *usecasex.Recorder
	log     *eventLog
	catalog port.ProductCatalog
	seen    *observation
}

// newSetStockFixture is the real provider wiring on real Postgres, with a spy on the transactor
// and on the catalog. The probe measures the fixture's own pool, the one the repository and the
// transactor use.
func newSetStockFixture(t *testing.T, products *memory.ProductCatalog) setStockFixture {
	t.Helper()
	c := context.Background()
	cfg := config.Postgres{URL: pgtest.URL(t)}
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(telCleanup)
	db, cleanup, err := platformpg.NewDB(cfg, tel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	if err := platformpg.Up(c, cfg, postgres.Migration()); err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(c).Exec(`DELETE FROM inventory.stocks WHERE sku LIKE 'SSC-%'`).Error; err != nil {
		t.Fatal(err)
	}
	rec, err := usecasex.NewRecorder(tel.Meter)
	if err != nil {
		t.Fatal(err)
	}

	f := setStockFixture{
		db:     db,
		stocks: ProvideStockRepository(db, tel.Logger),
		inbox:  ProvideInbox(db),
		rec:    rec,
		log:    &eventLog{},
		seen:   &observation{},
	}
	f.tx = spyTransactor{Transactor: ProvideTransactor(db), log: f.log}
	f.catalog = spyCatalog{inner: products, log: f.log, probe: func(c context.Context) { f.observe(t, c) }}

	return f
}

// observe records the state of the pool and of the database at this instant. inUse is read
// before the probe's own query takes a connection.
func (f setStockFixture) observe(t *testing.T, c context.Context) {
	t.Helper()
	f.seen.inTx = c.Value(inTxKey{}) != nil
	sqlDB, err := f.db.DB()
	if err != nil {
		t.Error(err)

		return
	}
	f.seen.inUse = sqlDB.Stats().InUse
	if err := f.db.WithContext(c).Raw(`SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid() AND state IN ('idle in transaction', 'idle in transaction (aborted)')`).Scan(&f.seen.openTx).Error; err != nil {
		t.Error(err)
	}
}

// provided is SetStock as the provider builds it.
func (f setStockFixture) provided() usecasex.UseCase[dto.SetStockInput, *dto.StockOutput] {
	return ProvideSetStock(f.stocks, f.tx, f.inbox, system.NewClock(), f.catalog, f.rec)
}

// guardInsideTx is the mistake the first test exists to catch: the same pieces, with the catalog
// asked from inside the transaction.
func (f setStockFixture) guardInsideTx() usecasex.UseCase[dto.SetStockInput, *dto.StockOutput] {
	return usecasex.Chain[dto.SetStockInput, *dto.StockOutput](usecase.NewSetStock(f.stocks, system.NewClock()),
		usecasex.Transactional[dto.SetStockInput, *dto.StockOutput](f.tx),
		requireProduct(f.catalog),
	)
}

// The catalog is a network call away (after #320): it must be asked before the transaction
// opens, not while a database connection is held waiting for it. The observer reads the pool the
// use case writes through; the second half proves the observer can fail, by asking from inside
// the transaction.
func TestSetStock_HoldsNoConnectionWhileAskingTheCatalog(t *testing.T) {
	c := context.Background()
	f := newSetStockFixture(t, memory.NewProductCatalog("SSC-HOLD"))

	if _, err := f.provided().Execute(c, dto.SetStockInput{SKU: "SSC-HOLD", OnHand: 3}); err != nil {
		t.Fatal(err)
	}
	if got := f.log.snapshot(); len(got) != 2 || got[0] != "ask" || got[1] != "tx" {
		t.Fatalf("order of events = %v, want [ask tx]", got)
	}
	if f.seen.inTx {
		t.Error("the catalog was asked inside the transaction's context")
	}
	if f.seen.inUse != 0 {
		t.Errorf("%d connections were checked out while the catalog was asked, want 0", f.seen.inUse)
	}
	if f.seen.openTx != 0 {
		t.Errorf("%d backends sat in an open transaction while the catalog was asked, want 0", f.seen.openTx)
	}

	// Fault injection: the same observer, with the guard inside the transaction, must go red.
	bad := newSetStockFixture(t, memory.NewProductCatalog("SSC-HOLD"))
	if _, err := bad.guardInsideTx().Execute(c, dto.SetStockInput{SKU: "SSC-HOLD", OnHand: 4}); err != nil {
		t.Fatal(err)
	}
	if got := bad.log.snapshot(); len(got) != 2 || got[0] != "tx" || got[1] != "ask" {
		t.Fatalf("faulty chain: order of events = %v, want [tx ask]", got)
	}
	if !bad.seen.inTx || bad.seen.inUse < 1 || bad.seen.openTx < 1 {
		t.Fatalf("faulty chain: observer saw %+v; it must see a transaction, a checked-out connection and an open backend", *bad.seen)
	}
}

// A refusal never opens a transaction, never writes, and keeps its meaning to the caller.
func TestSetStock_RefusalsOpenNoTransactionAndWriteNothing(t *testing.T) {
	c := context.Background()
	down := errors.New("catalog unreachable")

	t.Run("unknown sku", func(t *testing.T) {
		f := newSetStockFixture(t, memory.NewProductCatalog())
		_, err := f.provided().Execute(c, dto.SetStockInput{SKU: "SSC-NOPE", OnHand: 1})
		if !errors.Is(err, stock.ErrUnknownSKU) {
			t.Fatalf("err = %v, want ErrUnknownSKU", err)
		}
		f.expectNoTxNoRow(t, "SSC-NOPE")
	})

	t.Run("catalog fails", func(t *testing.T) {
		f := newSetStockFixture(t, memory.NewProductCatalog("SSC-DOWN").FailWith(down))
		_, err := f.provided().Execute(c, dto.SetStockInput{SKU: "SSC-DOWN", OnHand: 1})
		if !errors.Is(err, down) {
			t.Fatalf("err = %v, want the catalog's error kept", err)
		}
		f.expectNoTxNoRow(t, "SSC-DOWN")
	})

	// A malformed SKU is the domain's to refuse, and the catalog is not asked about it.
	for name, sku := range map[string]string{"empty": "", "65 bytes": string(make([]byte, 65))} {
		t.Run("invalid sku "+name, func(t *testing.T) {
			products := memory.NewAnyProductCatalog()
			f := newSetStockFixture(t, products)
			_, err := f.provided().Execute(c, dto.SetStockInput{SKU: sku, OnHand: 1})
			if !errors.Is(err, stock.ErrInvalidSKU) {
				t.Fatalf("err = %v, want ErrInvalidSKU", err)
			}
			if products.Calls() != 0 || len(f.log.snapshot()) != 0 {
				t.Fatalf("a malformed sku reached the catalog (%d calls) or a transaction (%v)", products.Calls(), f.log.snapshot())
			}
		})
	}
}

func (f setStockFixture) expectNoTxNoRow(t *testing.T, sku string) {
	t.Helper()
	if got := f.log.snapshot(); len(got) != 1 || got[0] != "ask" {
		t.Fatalf("events = %v, want only the catalog being asked", got)
	}
	var n int64
	if err := f.db.Raw(`SELECT count(*) FROM inventory.stocks WHERE sku = ?`, sku).Scan(&n).Error; err != nil || n != 0 {
		t.Fatalf("stock rows for %s = %d, %v; want none", sku, n, err)
	}
}
