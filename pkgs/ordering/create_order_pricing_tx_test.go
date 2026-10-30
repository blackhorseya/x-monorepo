package ordering_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/metrics"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/ownership"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/system"
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

// spyQuoter answers from inner and runs probe at the moment the catalog is asked.
type spyQuoter struct {
	inner port.PriceQuoter
	log   *eventLog
	probe func(c context.Context, db *gorm.DB)
	db    *gorm.DB
}

func (s spyQuoter) QuotePrices(c context.Context, skus []string) (*dto.PriceQuote, error) {
	s.log.add("quote")
	if s.probe != nil {
		s.probe(c, s.db)
	}

	return s.inner.QuotePrices(c, skus)
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

func (l *eventLog) count(e string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, got := range l.events {
		if got == e {
			n++
		}
	}

	return n
}

func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]string(nil), l.events...)
}

type pricingTxFixture struct {
	create func(c context.Context, in dto.CreateOrderInput) (*dto.OrderOutput, error)
	log    *eventLog
	prices *memory.PriceList
}

// newPricingTxFixture wires CreateOrder through the provider on real Postgres, with a spy
// on the transactor and on the quoter. probe runs inside every QuotePrices call with the
// fixture's own pool, the one the repository and the transactor use.
func newPricingTxFixture(t *testing.T, probe func(c context.Context, db *gorm.DB)) pricingTxFixture {
	t.Helper()
	_, db, tel := openDB(t)
	rec, err := usecasex.NewRecorder(tel.Meter)
	if err != nil {
		t.Fatal(err)
	}
	results, err := metrics.NewCreateOrderResults(sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader())))
	if err != nil {
		t.Fatal(err)
	}

	log := &eventLog{}
	prices := memory.NewPriceList().Set("A", 100, "USD")
	uc := ordering.ProvideCreateOrder(
		ordering.ProvideOrderRepository(db, tel.Logger),
		spyTransactor{Transactor: ordering.ProvideTransactor(db), log: log},
		ordering.ProvideInbox(db),
		publisher{},
		system.NewUUIDGenerator(),
		system.NewClock(),
		ownership.New("admin"),
		spyQuoter{inner: prices, log: log, probe: probe, db: db},
		rec,
		results,
	)

	return pricingTxFixture{create: uc.Execute, log: log, prices: prices}
}

func createA(customer, key string) dto.CreateOrderInput {
	return dto.CreateOrderInput{CustomerID: customer, IdempotencyKey: key, Items: []dto.OrderLine{{SKU: "A", Quantity: 1}}}
}

// The catalog is a network call away: it must be asked before the transaction opens, not
// while a database connection is held waiting for it.
func TestCreateOrder_PricesBeforeTheTransactionOpens(t *testing.T) {
	f := newPricingTxFixture(t, func(c context.Context, _ *gorm.DB) {
		if c.Value(inTxKey{}) != nil {
			t.Error("the catalog was asked inside the transaction's context")
		}
	})

	if _, err := f.create(context.Background(), createA("c-ptx-order", "")); err != nil {
		t.Fatal(err)
	}
	if got := f.log.snapshot(); len(got) != 2 || got[0] != "quote" || got[1] != "tx" {
		t.Fatalf("order of events = %v, want [quote tx]", got)
	}
}

// While the catalog is being asked, no connection of the pool is checked out and no
// backend sits in an open transaction: a slow catalog cannot starve the database.
func TestCreateOrder_HoldsNoConnectionWhilePricing(t *testing.T) {
	var inUse, openTx int
	f := newPricingTxFixture(t, func(c context.Context, db *gorm.DB) {
		sqlDB, err := db.DB()
		if err != nil {
			t.Error(err)

			return
		}
		inUse = sqlDB.Stats().InUse
		if err := db.WithContext(c).Raw(`SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid() AND state IN ('idle in transaction', 'idle in transaction (aborted)')`).Scan(&openTx).Error; err != nil {
			t.Error(err)
		}
	})

	if _, err := f.create(context.Background(), createA("c-ptx-conn", "")); err != nil {
		t.Fatal(err)
	}
	if f.log.count("quote") != 1 {
		t.Fatalf("the catalog was asked %d times, want 1", f.log.count("quote"))
	}
	if inUse != 0 {
		t.Fatalf("%d connections were checked out while the catalog was asked, want 0", inUse)
	}
	if openTx != 0 {
		t.Fatalf("%d backends sat in an open transaction while the catalog was asked, want 0", openTx)
	}
}

// A retry that already created its order is answered before the catalog is asked and
// before any transaction opens.
func TestCreateOrder_ReplayOpensNoTransactionAndAsksNoCatalog(t *testing.T) {
	f := newPricingTxFixture(t, nil)
	c := context.Background()

	first, err := f.create(c, createA("c-ptx-replay", "k-replay"))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.log.snapshot(); len(got) != 2 {
		t.Fatalf("first create events = %v, want [quote tx]", got)
	}

	again, err := f.create(c, createA("c-ptx-replay", "k-replay"))
	if err != nil || !again.Replayed || again.ID != first.ID {
		t.Fatalf("replay = %+v, %v; want the original order, replayed", again, err)
	}
	if got := f.log.snapshot(); len(got) != 2 {
		t.Fatalf("events after the replay = %v, want it to add neither a quote nor a tx", got)
	}

	// Reuse with other lines is refused from the same early read.
	other := createA("c-ptx-replay", "k-replay")
	other.Items[0].Quantity = 2
	if _, err := f.create(c, other); !errors.Is(err, order.ErrIdempotencyKeyReused) {
		t.Fatalf("reuse err = %v, want ErrIdempotencyKeyReused", err)
	}
	if got := f.log.snapshot(); len(got) != 2 {
		t.Fatalf("events after the reuse = %v, want no new quote or tx", got)
	}
}

// A SKU the catalog cannot quote refuses the order before any transaction opens, and
// does not spend the key: the same key works once the SKU can be quoted.
func TestCreateOrder_UnquotableSKUOpensNoTransactionAndKeepsTheKey(t *testing.T) {
	f := newPricingTxFixture(t, nil)
	c := context.Background()

	bad := createA("c-ptx-key", "k-key")
	bad.Items[0].SKU = "NOPE"
	if _, err := f.create(c, bad); !errors.Is(err, order.ErrUnknownSKU) {
		t.Fatalf("err = %v, want ErrUnknownSKU", err)
	}
	if f.log.count("tx") != 0 {
		t.Fatalf("a transaction opened for an order the catalog refused")
	}

	f.prices.Set("NOPE", 300, "USD")
	out, err := f.create(c, bad)
	if err != nil || out.Replayed {
		t.Fatalf("retry with the same key = %+v, %v; want a new order", out, err)
	}
}
