package inventory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/adapters/consumer"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/stock"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

type clockAt struct{ at time.Time }

func (x clockAt) Now() time.Time { return x.at }

// beforeFind runs hook once, just before the first stock load: the point
// between a use case reading its reservation and reading the stock.
type beforeFind struct {
	stock.Repository
	hook func()
}

func (x *beforeFind) FindBySKU(c context.Context, sku stock.SKU) (*stock.Stock, error) {
	if h := x.hook; h != nil {
		x.hook = nil
		h()
	}

	return x.Repository.FindBySKU(c, sku)
}

type raceEnv struct {
	c            context.Context
	stocks       stock.Repository
	reservations reservation.Repository
	tx           port.Transactor
	inbox        port.Inbox
	rec          *usecasex.Recorder
	now, old     clockAt
}

// newRaceEnv opens a database with SKU-RACE stocked with 2 and o-race-1
// reserved (2 units) 25 hours ago, i.e. due for the expiry sweep.
func newRaceEnv(t *testing.T) *raceEnv {
	t.Helper()
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
	t.Cleanup(cleanup)
	if err := platformpg.Up(c, cfg, postgres.Migration()); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DELETE FROM inventory.reservations WHERE order_id LIKE 'o-race-%'`,
		`DELETE FROM inventory.stocks WHERE sku = 'SKU-RACE'`,
	} {
		if err := db.WithContext(c).Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}

	rec, err := usecasex.NewRecorder(tel.Meter)
	if err != nil {
		t.Fatal(err)
	}
	e := &raceEnv{
		c:            c,
		stocks:       inventory.ProvideStockRepository(db, tel.Logger),
		reservations: inventory.ProvideReservationRepository(db),
		tx:           inventory.ProvideTransactor(db),
		inbox:        inventory.ProvideInbox(db),
		rec:          rec,
		now:          clockAt{at: time.Now().UTC()},
	}
	e.old = clockAt{at: e.now.at.Add(-25 * time.Hour)}
	if _, err := inventory.ProvideSetStock(e.stocks, e.tx, e.inbox, e.now, rec).Execute(c, dto.SetStockInput{SKU: "SKU-RACE", OnHand: 2}); err != nil {
		t.Fatal(err)
	}
	e.reserve(t, "o-race-1", e.old)

	return e
}

func (e *raceEnv) reserve(t *testing.T, orderID string, clock clockAt) {
	t.Helper()
	uc := inventory.ProvideReserveStock(e.reservations, e.stocks, e.tx, e.inbox, publisher{}, clock, e.rec)
	out, err := uc.Execute(e.c, dto.ReserveStockInput{OrderID: orderID, Lines: []dto.Line{{SKU: "SKU-RACE", Quantity: 2}}})
	if err != nil || out.Status != "RESERVED" {
		t.Fatalf("reserve %s = %+v, %v", orderID, out, err)
	}
}

func (e *raceEnv) expire(t *testing.T) {
	t.Helper()
	out, err := inventory.ProvideExpireReservations(e.reservations, e.stocks, e.tx, e.inbox, publisher{}, e.now, e.rec).Execute(e.c, dto.ExpireReservationsInput{OlderThan: 24 * time.Hour})
	if err != nil || len(out.Expired) != 1 {
		t.Fatalf("expire = %+v, %v", out, err)
	}
}

func (e *raceEnv) commit(stocks stock.Repository, orderID string) (*dto.ReservationOutput, error) {
	return inventory.ProvideCommitStock(e.reservations, stocks, e.tx, e.inbox, publisher{}, e.now, e.rec).Execute(e.c, dto.CommitStockInput{OrderID: orderID})
}

func (e *raceEnv) stockNow(t *testing.T) *stock.Stock {
	t.Helper()
	s, err := e.stocks.FindBySKU(e.c, "SKU-RACE")
	if err != nil {
		t.Fatal(err)
	}

	return s
}

// The stock version cannot see a reservation that changed after it was read:
// CommitStock reads o1 as RESERVED, then another transaction expires o1 and
// o2 reserves the very units it gave back, and only then does CommitStock
// load the (now fresh) stock. Without a lock on the reservation itself it
// would sell o1 out of o2's hold. The commit must lose (retryable), and the
// retry must see EXPIRED, find nothing available and end COMMIT_SHORT, with
// o2's hold intact.
func TestCommitStock_ReservationChangedUnderneath(t *testing.T) {
	e := newRaceEnv(t)
	interleaved := &beforeFind{Repository: e.stocks}
	interleaved.hook = func() {
		e.expire(t)
		e.reserve(t, "o-race-2", e.now)
	}

	out, err := e.commit(interleaved, "o-race-1")
	if !errors.Is(err, kernel.ErrConcurrentUpdate) {
		t.Fatalf("commit over a changed reservation = %+v, %v; want a retryable concurrent update", out, err)
	}

	// The failed attempt rolled back; the retry reloads and sees EXPIRED.
	out, err = e.commit(e.stocks, "o-race-1")
	if err != nil || out.Status != "COMMIT_SHORT" {
		t.Fatalf("retry = %+v, %v; want COMMIT_SHORT", out, err)
	}
	if s := e.stockNow(t); s.OnHand() != 2 || s.Reserved() != 2 {
		t.Fatalf("stock = on_hand %d reserved %d; want 2/2 (o2's hold intact)", s.OnHand(), s.Reserved())
	}
	out, err = e.commit(e.stocks, "o-race-2")
	if err != nil || out.Status != "COMMITTED" {
		t.Fatalf("o2 commit = %+v, %v; want COMMITTED", out, err)
	}
}

// Same window, nobody reserves the units the sweep gave back: the stock the
// commit loads now says reserved 0, so selling "from the reservation" fails
// in the stock aggregate before the reservation is ever written. That is the
// same lost race, not a final domain error: it must be retryable, and the
// retry sells o1 from what is available (COMMITTED).
func TestCommitStock_ExpiredUnderneathWithoutRereserve(t *testing.T) {
	e := newRaceEnv(t)
	interleaved := &beforeFind{Repository: e.stocks}
	interleaved.hook = func() { e.expire(t) }

	out, err := e.commit(interleaved, "o-race-1")
	if !errors.Is(err, kernel.ErrConcurrentUpdate) {
		t.Fatalf("commit over an expired reservation = %+v, %v; want a retryable concurrent update, not a final domain error", out, err)
	}

	out, err = e.commit(e.stocks, "o-race-1")
	if err != nil || out.Status != "COMMITTED" {
		t.Fatalf("retry = %+v, %v; want COMMITTED", out, err)
	}
	if s := e.stockNow(t); s.OnHand() != 0 || s.Reserved() != 0 {
		t.Fatalf("stock = on_hand %d reserved %d; want 0/0", s.OnHand(), s.Reserved())
	}
}

// The same race as TestCommitStock_ExpiredUnderneathWithoutRereserve, through
// the consumer entry point: the OrderCompleted handler must not turn it into a
// drop (the eventbus would never deliver the message again, order COMPLETED
// and nothing sold). A plain error is retried, and the retry sells.
func TestOrderCompletedHandler_ExpiredUnderneathIsRetried(t *testing.T) {
	e := newRaceEnv(t)
	interleaved := &beforeFind{Repository: e.stocks}
	interleaved.hook = func() { e.expire(t) }
	payload, err := proto.Marshal(&orderingv1.OrderCompleted{OrderId: "o-race-1"})
	if err != nil {
		t.Fatal(err)
	}
	handle := func(stocks stock.Repository) error {
		commit := inventory.ProvideCommitStock(e.reservations, stocks, e.tx, e.inbox, publisher{}, e.now, e.rec)

		return consumer.NewOrderCompletedHandler(commit).Handle(e.c, payload)
	}

	err = handle(interleaved)
	if err == nil || errors.Is(err, consumer.ErrDrop) || !errors.Is(err, kernel.ErrConcurrentUpdate) {
		t.Fatalf("handler = %v, want a retryable error (not ErrDrop)", err)
	}
	if err := handle(e.stocks); err != nil {
		t.Fatalf("retry: %v", err)
	}
	got, err := e.reservations.FindByOrderID(e.c, "o-race-1")
	if err != nil || got.Status() != reservation.StatusCommitted {
		t.Fatalf("reservation = %v, %v; want COMMITTED", got, err)
	}
	if s := e.stockNow(t); s.OnHand() != 0 || s.Reserved() != 0 {
		t.Fatalf("stock = on_hand %d reserved %d; want 0/0", s.OnHand(), s.Reserved())
	}
}
