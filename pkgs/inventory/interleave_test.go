package inventory_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/protobuf/proto"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/adapters/consumer"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/stock"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// Interleaving regression matrix (issue #231) for the inventory transitions
// that race on one reservation: CommitStock (OrderCompleted), ReleaseStock
// (OrderCancelled) and ExpireReservations, against real Postgres through the
// provider's transaction / inbox / metrics decorators and, for the consumers,
// through the real handlers (so "retry" vs "drop" is the eventbus's answer,
// not just an error type).
//
// Every interleaving is forced, never timed. The victim runs normally; at the
// chosen barrier a hook runs the intruder to completion (its own committed
// transaction) and only then lets the victim continue:
//
//	bFind   after the victim read the reservation, before it reads the stock
//	bSave   after it read the stock and decided, before it writes the stock
//	bUpdate after it wrote the stock, before it writes the reservation
//
// bUpdate is different: the victim's transaction already holds the stock row
// lock, so an intruder that touches the stock would block the hook forever. The
// intruder runs in a goroutine instead and the hook waits until Postgres
// reports it parked on a lock (pg_stat_activity), the victim then finishes and
// the lock releases it. That wait polls an observable state; nothing is slept
// for as a guess.
//
// What each lock is proven by (see the cell comments):
//   - stock version (Save WHERE version): intruders that change only the stock
//     (a spare-units reserve) at bSave;
//   - reservation status CAS (Update WHERE status): intruders that hand the
//     victim a *fresh* stock which fits its stale reservation, at bFind;
//   - staleIfMoved: every bFind cell where the aggregate's own domain error
//     comes first, asserted through the handler as retry, never ErrDrop.

type outcome string

const (
	outOK    outcome = "ok"    // nil: acked
	outDrop  outcome = "drop"  // eventbus.ErrDrop: acked and dropped for good
	outStale outcome = "stale" // retried: wraps kernel.ErrConcurrentUpdate, not a kernel.Error
)

func classify(err error) outcome {
	switch {
	case err == nil:
		return outOK
	case errors.Is(err, consumer.ErrDrop):
		return outDrop
	case errors.Is(err, kernel.ErrConcurrentUpdate):
		return outStale
	default:
		return outcome("other: " + err.Error())
	}
}

type victim int

const (
	vComplete victim = iota // OrderCompleted → CommitStock
	vCancel                 // OrderCancelled → ReleaseStock
	vExpire                 // ExpireReservations sweep
)

func (v victim) usecase() string {
	return [...]string{"inventory.commit_stock", "inventory.release_stock", "inventory.expire_reservations"}[v]
}

type barrier int

const (
	bFind barrier = iota
	bSave
	bUpdate
)

type clockAt struct{ at time.Time }

func (x clockAt) Now() time.Time { return x.at }

// hookStocks runs onFind / onSave once, just before the first FindBySKU / Save.
type hookStocks struct {
	stock.Repository
	onFind, onSave func()
}

func (x *hookStocks) FindBySKU(c context.Context, sku stock.SKU) (*stock.Stock, error) {
	if f := x.onFind; f != nil {
		x.onFind = nil
		f()
	}

	return x.Repository.FindBySKU(c, sku)
}

func (x *hookStocks) Save(c context.Context, s *stock.Stock) error {
	if f := x.onSave; f != nil {
		x.onSave = nil
		f()
	}

	return x.Repository.Save(c, s)
}

// hookReservations runs onUpdate once, just before the first Update.
type hookReservations struct {
	reservation.Repository
	onUpdate func(c context.Context)
}

func (x *hookReservations) Update(c context.Context, r *reservation.Reservation) error {
	if f := x.onUpdate; f != nil {
		x.onUpdate = nil
		f(c)
	}

	return x.Repository.Update(c, r)
}

// scopedSweep limits the expiry sweep's DB-wide read to one cell's orders. The
// filter runs after an unlimited read, so rows of other fixtures neither get
// expired nor crowd this cell out of the sweep limit.
type scopedSweep struct {
	reservation.Repository
	prefix string
}

func (x scopedSweep) ListReservedBefore(c context.Context, before time.Time, limit int) ([]*reservation.Reservation, error) {
	all, err := x.Repository.ListReservedBefore(c, before, 0)
	if err != nil {
		return nil, err
	}
	var mine []*reservation.Reservation
	for _, r := range all {
		if strings.HasPrefix(r.OrderID(), x.prefix) {
			mine = append(mine, r)
		}
	}
	if limit > 0 && len(mine) > limit {
		mine = mine[:limit]
	}

	return mine, nil
}

var envSeq atomic.Int64

type raceEnv struct {
	t            *testing.T
	c            context.Context
	db           *gorm.DB
	logger       *slog.Logger
	stocks       stock.Repository
	reservations reservation.Repository
	tx           port.Transactor
	inbox        port.Inbox
	rec          *usecasex.Recorder
	reader       *metric.ManualReader
	now, old     clockAt

	// Every row of one cell is named after p, so cells never share data (the
	// expiry sweep is the only DB-wide read; attempt wraps it in scopedSweep).
	p, sku, skuB string
	o1, o2       string

	// startGate, when set, runs in pinned before the caller starts waiting for
	// its actor, with the actor's result channel: tests use it to force the
	// schedule in which the actor has already finished.
	startGate func(result <-chan error)

	expiredMine int // how many of o1 the last ExpireReservations attempt expired
}

// withServerTimeouts adds server-side lock and statement timeouts to a
// connection URL. Contexts bound what this package's own calls do, but
// platformpg.Up runs migrate, whose Version query ignores the caller's context
// (migrate v4 pgx driver, QueryRowContext(context.Background())); the server is
// the one place that bounds it. Connection establishment is not covered.
func withServerTimeouts(raw string, lock, statement time.Duration) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	// pgx sends unknown DSN keys to the server as runtime parameters (milliseconds).
	q.Set("lock_timeout", fmt.Sprint(lock.Milliseconds()))
	q.Set("statement_timeout", fmt.Sprint(statement.Milliseconds()))
	u.RawQuery = q.Encode()

	return u.String()
}

// newEnv opens the database with SKU (on_hand onHand) and no reservation.
func newEnv(t *testing.T, onHand int32) *raceEnv {
	t.Helper()
	url := pgtest.URL(t)

	// The test's own statements, the victim included, run under this deadline;
	// migrate's and the connection dial are bounded by withServerTimeouts / pgx
	// instead, and cleanup gets a fresh budget of its own.
	c, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	cfg := config.Postgres{URL: withServerTimeouts(url, 30*time.Second, 2*time.Minute)}
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

	p := fmt.Sprintf("x%x%d", time.Now().UnixNano()&0xffffff, envSeq.Add(1))
	e := &raceEnv{
		t: t, c: c, db: db, logger: tel.Logger,
		p: p, sku: "SKU-" + p, skuB: "SKUB-" + p,
		o1: "o-" + p + "-1", o2: "o-" + p + "-2",
	}
	clean := func() {
		cc, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		like := "%" + p + "%"
		for _, q := range []string{
			`DELETE FROM inventory.reservations WHERE order_id LIKE ?`,
			`DELETE FROM inventory.stocks WHERE sku LIKE ?`,
			`DELETE FROM inventory.inbox WHERE message_id LIKE ?`,
		} {
			if err := db.WithContext(cc).Exec(q, like).Error; err != nil {
				t.Errorf("cleanup %q: %v", q, err)
			}
		}
	}
	clean()
	t.Cleanup(clean)

	reader := metric.NewManualReader()
	rec, err := usecasex.NewRecorder(metric.NewMeterProvider(metric.WithReader(reader)))
	if err != nil {
		t.Fatal(err)
	}
	e.rec, e.reader = rec, reader
	e.stocks = inventory.ProvideStockRepository(db, tel.Logger)
	e.reservations = inventory.ProvideReservationRepository(db)
	e.tx = inventory.ProvideTransactor(db)
	e.inbox = inventory.ProvideInbox(db)
	e.now = clockAt{at: time.Now().UTC()}
	e.old = clockAt{at: e.now.at.Add(-25 * time.Hour)}
	e.setStock(e.sku, onHand)

	return e
}

// newRaceEnv is newEnv plus o1 holding 2 units of SKU, reserved 25 hours ago
// (due for the expiry sweep), so the stock starts at on_hand/2.
func newRaceEnv(t *testing.T, onHand int32) *raceEnv {
	t.Helper()
	e := newEnv(t, onHand)
	e.reserve(e.o1, e.old, dto.Line{SKU: e.sku, Quantity: 2})

	return e
}

func (e *raceEnv) setStock(sku string, onHand int32) {
	e.t.Helper()
	if _, err := inventory.ProvideSetStock(e.stocks, e.tx, e.inbox, e.now, memory.NewAnyProductCatalog(), e.rec).Execute(e.c, dto.SetStockInput{SKU: sku, OnHand: onHand}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *raceEnv) reserve(orderID string, clock clockAt, lines ...dto.Line) {
	e.t.Helper()
	uc := inventory.ProvideReserveStock(e.reservations, e.stocks, e.tx, e.inbox, publisher{}, clock, e.rec)
	out, err := uc.Execute(e.c, dto.ReserveStockInput{OrderID: orderID, Lines: lines})
	if err != nil || out.Status != "RESERVED" {
		e.t.Fatalf("reserve %s = %+v, %v", orderID, out, err)
	}
}

// attempt is one delivery of the victim against the given repositories:
// the consumer handler for complete / cancel (with message id msgID when set,
// as eventbus does), the use case for the sweep.
func (e *raceEnv) attempt(v victim, msgID string, stocks stock.Repository, reservations reservation.Repository) error {
	c := e.c
	if msgID != "" {
		c = contextx.WithMessageID(c, msgID)
	}
	switch v {
	case vComplete:
		payload, err := proto.Marshal(&orderingv1.OrderCompleted{OrderId: e.o1})
		if err != nil {
			e.t.Fatal(err)
		}

		return consumer.NewOrderCompletedHandler(inventory.ProvideCommitStock(reservations, stocks, e.tx, e.inbox, publisher{}, e.now, e.rec)).Handle(c, payload)
	case vCancel:
		payload, err := proto.Marshal(&orderingv1.OrderCancelled{OrderId: e.o1})
		if err != nil {
			e.t.Fatal(err)
		}

		return consumer.NewOrderCancelledHandler(inventory.ProvideReleaseStock(reservations, stocks, e.tx, e.inbox, publisher{}, e.now, e.rec)).Handle(c, payload)
	default:
		out, err := inventory.ProvideExpireReservations(scopedSweep{Repository: reservations, prefix: "o-" + e.p + "-"}, stocks, e.tx, e.inbox, publisher{}, e.now, e.rec).Execute(c, dto.ExpireReservationsInput{OlderThan: 24 * time.Hour})
		e.expiredMine = 0
		if out != nil {
			for _, x := range out.Expired {
				if x.OrderID == e.o1 {
					e.expiredMine++
				}
			}
		}

		return err
	}
}

// An intruder is another actor moving the same reservation / stock; it runs as
// its own transactions and reports its error instead of failing the test, so it
// can also run off the test goroutine.
type intruder func(e *raceEnv) error

var (
	// iExpire: the sweep gives o1's stock back (EXPIRED, stock x/0).
	iExpire intruder = func(e *raceEnv) error {
		if err := e.attempt(vExpire, "", e.stocks, e.reservations); err != nil {
			return err
		}
		if e.expiredMine != 1 {
			return fmt.Errorf("sweep expired %d of o1; want exactly 1", e.expiredMine)
		}

		return nil
	}
	// iCommit: the order completes first.
	iCommit intruder = func(e *raceEnv) error { return e.attempt(vComplete, "", e.stocks, e.reservations) }
	// iRelease: the order is cancelled first.
	iRelease intruder = func(e *raceEnv) error { return e.attempt(vCancel, "", e.stocks, e.reservations) }
	// iReserve: another order takes spare units (changes only the stock row).
	iReserve intruder = func(e *raceEnv) error { return e.tryReserve(e.o2, e.now, dto.Line{SKU: e.sku, Quantity: 2}) }
)

// then runs a, then b: "o1 gave its units back, and o2 reserved them".
func then(a, b intruder) intruder {
	return func(e *raceEnv) error {
		if err := a(e); err != nil {
			return err
		}

		return b(e)
	}
}

func (e *raceEnv) tryReserve(orderID string, clock clockAt, lines ...dto.Line) error {
	uc := inventory.ProvideReserveStock(e.reservations, e.stocks, e.tx, e.inbox, publisher{}, clock, e.rec)
	out, err := uc.Execute(e.c, dto.ReserveStockInput{OrderID: orderID, Lines: lines})
	if err != nil {
		return err
	}
	if out.Status != "RESERVED" {
		return fmt.Errorf("reserve %s = %s", orderID, out.Status)
	}

	return nil
}

// bounded is a context for one observer or actor step: every statement the
// matrix issues outside the provider has an end, so a stuck connection fails
// the test instead of hanging it.
func (e *raceEnv) bounded(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(e.c, d)
}

// victimPID identifies the victim's backend from inside its transaction context.
func (e *raceEnv) victimPID(c context.Context) int {
	e.t.Helper()
	ctx, cancel := e.bounded(10 * time.Second)
	defer cancel()
	var pid int
	if err := platformpg.TxOrDB(c, e.db).WithContext(ctx).Raw(`SELECT pg_backend_pid()`).Scan(&pid).Error; err != nil {
		e.t.Fatal(err)
	}

	return pid
}

// blockedBy is how many sessions wait for a lock held by backend holder:
// waiter is the one session that counts, or 0 for any.
func (e *raceEnv) blockedBy(holder, waiter int) int {
	e.t.Helper()
	ctx, cancel := e.bounded(10 * time.Second)
	defer cancel()
	var n int
	if err := e.db.WithContext(ctx).Raw(`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND ? = ANY(pg_blocking_pids(pid)) AND (? = 0 OR pid = ?)`, holder, waiter, waiter).Scan(&n).Error; err != nil {
		e.t.Fatal(err)
	}

	return n
}

// on is e with its repositories, transactor and inbox bound to one pinned
// connection, so everything run through it is one Postgres backend, and with
// ctx as its context: the provider re-applies it on every statement, so the
// deadline survives into the use case.
func (e *raceEnv) on(conn *gorm.DB, ctx context.Context) *raceEnv {
	c := *e
	c.c = ctx
	c.db = conn
	c.stocks = inventory.ProvideStockRepository(conn, e.logger)
	c.reservations = inventory.ProvideReservationRepository(conn)
	c.tx = inventory.ProvideTransactor(conn)
	c.inbox = inventory.ProvideInbox(conn)

	return &c
}

// pinned runs fn on its own goroutine, on one pinned connection, under a
// context that ends after a minute or when the test does (so a Fatal elsewhere
// never strands it). It returns that connection's backend pid and the channel
// fn's result arrives on. Every way the start can fail (no connection, pid
// query error, 15s) comes back as an error instead of a hang.
func (e *raceEnv) pinned(fn func(ctx context.Context, conn *gorm.DB) error) (int, <-chan error, error) {
	ctx, cancel := e.bounded(time.Minute)
	e.t.Cleanup(cancel)
	result := make(chan error, 1)
	pids := make(chan int, 1)
	// fire-and-forget: joined by the caller through result; ctx is cancelled at test end.
	go func() {
		result <- e.db.WithContext(ctx).Connection(func(conn *gorm.DB) error {
			conn = conn.WithContext(ctx)
			var pid int
			if err := conn.Raw(`SELECT pg_backend_pid()`).Scan(&pid).Error; err != nil {
				return err
			}
			pids <- pid

			return fn(ctx, conn)
		})
	}()

	if e.startGate != nil {
		e.startGate(result)
	}
	select {
	case pid := <-pids:
		return pid, result, nil
	case err := <-result:
		// A fast actor can publish its pid and its result before this select
		// runs, and select picks at random: a published pid means it started.
		select {
		case pid := <-pids:
			done := make(chan error, 1)
			done <- err

			return pid, done, nil
		default:
		}

		return 0, nil, fmt.Errorf("pinned actor did not start: %w", err)
	case <-time.After(15 * time.Second):
		cancel()

		return 0, nil, errors.New("pinned actor did not start within 15s")
	}
}

// startIntruder is pinned running in against this environment.
func (e *raceEnv) startIntruder(in intruder) (int, <-chan error, error) {
	return e.pinned(func(ctx context.Context, conn *gorm.DB) error { return in(e.on(conn, ctx)) })
}

// await waits for ready, failing with the actor's own error if it ends first.
func (e *raceEnv) await(ready <-chan struct{}, done <-chan error) {
	e.t.Helper()
	select {
	case <-ready:
	case err := <-done:
		e.t.Fatalf("actor ended (%v) before it was ready", err)
	case <-time.After(15 * time.Second):
		e.t.Fatal("actor was not ready within 15s")
	}
}

// join waits for an actor's result.
func (e *raceEnv) join(done <-chan error) error {
	e.t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		e.t.Fatal("actor did not finish within 30s")

		return nil
	}
}

// gate blocks until open is closed or the actor's context ends.
func gate(ctx context.Context, open <-chan struct{}) error {
	select {
	case <-open:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// parked starts in and returns once Postgres reports exactly that backend
// waiting on a lock held by the victim's backend (c is the victim's
// transaction context) — the intruder has done its reads and reached its
// write. Waiters on anything else, or any other session on the victim, do
// not count. The channel carries the intruder's result.
func (e *raceEnv) parked(c context.Context, in intruder) <-chan error {
	victim := e.victimPID(c)
	intruderPID, done, err := e.startIntruder(in)
	if err != nil {
		e.t.Fatal(err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for e.blockedBy(victim, intruderPID) == 0 {
		select {
		case err := <-done:
			e.t.Fatalf("intruder finished (%v) instead of waiting for the victim's lock", err)
		default:
		}
		if time.Now().After(deadline) {
			e.t.Fatal("intruder never blocked on the victim's lock")
		}
		time.Sleep(time.Millisecond) // poll interval of an observed condition, not a guess at timing
	}

	return done
}

type state struct {
	o1, o2   reservation.Status
	onHand   int32
	reserved int32
}

func (e *raceEnv) read(withO2 bool) state {
	e.t.Helper()
	var st state
	r, err := e.reservations.FindByOrderID(e.c, e.o1)
	if err != nil {
		e.t.Fatal(err)
	}
	st.o1 = r.Status()
	if withO2 {
		r2, err := e.reservations.FindByOrderID(e.c, e.o2)
		if err != nil {
			e.t.Fatal(err)
		}
		st.o2 = r2.Status()
	}
	s, err := e.stocks.FindBySKU(e.c, stock.SKU(e.sku))
	if err != nil {
		e.t.Fatal(err)
	}
	st.onHand, st.reserved = s.OnHand(), s.Reserved()
	// The invariant every interleaving must keep, whatever it decided.
	if st.reserved < 0 || st.onHand < st.reserved {
		e.t.Fatalf("stock broke 0 <= reserved <= on_hand: on_hand %d reserved %d", st.onHand, st.reserved)
	}

	return st
}

// aborted is the usecase.duration samples of kind "aborted" per use case.
func (e *raceEnv) aborted() map[string]int {
	e.t.Helper()
	var rm metricdata.ResourceMetrics
	if err := e.reader.Collect(e.c, &rm); err != nil {
		e.t.Fatal(err)
	}
	got := map[string]int{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			hist, ok := m.Data.(metricdata.Histogram[float64])
			if !ok || m.Name != "usecase.duration" {
				continue
			}
			for _, dp := range hist.DataPoints {
				name, _ := dp.Attributes.Value("usecase")
				kind, _ := dp.Attributes.Value("kind")
				if kind.AsString() == "aborted" {
					got[name.AsString()] += int(dp.Count)
				}
			}
		}
	}

	return got
}

// cell is one scheduled interleaving. The victim's first delivery meets the
// intruder at the barrier and must end `first`; the same message delivered again
// (eventbus redelivery) must end `retry`, and the final state must be `want`.
type cell struct {
	name   string
	onHand int32 // SKU on-hand before o1's hold; default 2
	// holdO2 reserves 2 units for o2 at e.now before the race (needs onHand 4).
	holdO2 bool

	victim victim
	at     barrier
	by     intruder

	first, retry outcome
	retryExpires int // vExpire: how many of o1 the redelivery expires

	want    state
	checkO2 bool
	// aborted overrides the expected aborted use-case counts; default is one for
	// the victim's use case when `first` is outStale.
	aborted map[string]int
}

func TestInterleavingMatrix(t *testing.T) {
	const (
		reserved  = reservation.StatusReserved
		committed = reservation.StatusCommitted
		short     = reservation.StatusCommitShort
		released  = reservation.StatusReleased
		expired   = reservation.StatusExpired
	)

	cells := []cell{
		// ── complete (CommitStock) vs expire ───────────────────────────────
		{
			// The sweep returned the units and nobody took them: the stock the
			// commit loads says reserved 0, so its own domain guard
			// (commit_exceeds_reserved) fires before any stale-token check.
			// That is a lost race, retryable; the retry sells o1 from what is
			// available.
			name: "complete/expire-without-rereserve@find", victim: vComplete, at: bFind, by: iExpire,
			first: outStale, retry: outOK, want: state{o1: committed, onHand: 0, reserved: 0},
		},
		{
			// …and o2 took them: the fresh stock (2/2) fits o1's stale RESERVED
			// snapshot, so only the reservation status CAS stops o1 selling out
			// of o2's hold. The retry sees EXPIRED, finds nothing available and
			// ends COMMIT_SHORT; o2's hold is intact.
			name: "complete/expire-then-o2-reserves@find", victim: vComplete, at: bFind, by: then(iExpire, iReserve),
			first: outStale, retry: outOK, want: state{o1: short, o2: reserved, onHand: 2, reserved: 2}, checkO2: true,
		},
		{
			// The sweep lands after the commit decided on the stock it read: the
			// stock version moved, Save loses.
			name: "complete/expire@save", victim: vComplete, at: bSave, by: iExpire,
			first: outStale, retry: outOK, want: state{o1: committed, onHand: 0, reserved: 0},
		},
		{
			// Only the stock changes (o2 takes the spare units; o1 stays
			// RESERVED, so the status CAS cannot see it). Without the version
			// predicate the commit would write its 2/0 over o2's 4/4.
			name: "complete/spare-units-reserved@save", onHand: 4, victim: vComplete, at: bSave, by: iReserve,
			first: outStale, retry: outOK, want: state{o1: committed, o2: reserved, onHand: 2, reserved: 2}, checkO2: true,
		},
		{
			// The commit holds the stock row lock when the sweep arrives. The
			// sweep waits, then loses its stock version and aborts; o1 is sold once.
			name: "complete/expire-blocked@update", victim: vComplete, at: bUpdate, by: iExpire,
			first: outOK, want: state{o1: committed, onHand: 0, reserved: 0},
			aborted: map[string]int{"inventory.expire_reservations": 1},
		},

		// ── complete vs release ────────────────────────────────────────────
		{
			// Ordering cannot emit both OrderCancelled and OrderCompleted, but
			// consumers are at-least-once and unordered: if both ever arrive the
			// second must not oversell. Cancel got there first.
			name: "complete/release@find", victim: vComplete, at: bFind, by: iRelease,
			first: outStale, retry: outDrop, want: state{o1: released, onHand: 2, reserved: 0},
		},
		{
			// …with the units re-reserved by o2: status CAS again.
			name: "complete/release-then-o2-reserves@find", victim: vComplete, at: bFind, by: then(iRelease, iReserve),
			first: outStale, retry: outDrop, want: state{o1: released, o2: reserved, onHand: 2, reserved: 2}, checkO2: true,
		},

		// ── double complete ────────────────────────────────────────────────
		{
			// Same order completed twice (two different messages): the second
			// finds the stock already sold and must neither deduct again nor go negative.
			name: "complete/complete@find", victim: vComplete, at: bFind, by: iCommit,
			first: outStale, retry: outDrop, want: state{o1: committed, onHand: 0, reserved: 0},
		},
		{
			// The same with o2 holding units: the fresh stock (2/2) fits the
			// stale snapshot, so a second deduction would eat o2's hold. Only the
			// status CAS prevents it.
			name: "complete/complete-with-o2-hold@find", onHand: 4, holdO2: true, victim: vComplete, at: bFind, by: iCommit,
			first: outStale, retry: outDrop, want: state{o1: committed, o2: reserved, onHand: 2, reserved: 2}, checkO2: true,
		},

		// ── cancel (ReleaseStock) vs expire / complete ─────────────────────
		{
			name: "cancel/expire@find", victim: vCancel, at: bFind, by: iExpire,
			first: outStale, retry: outDrop, want: state{o1: expired, onHand: 2, reserved: 0},
		},
		{
			// A release over o2's fresh hold would free o2's units; the CAS stops it.
			name: "cancel/expire-then-o2-reserves@find", victim: vCancel, at: bFind, by: then(iExpire, iReserve),
			first: outStale, retry: outDrop, want: state{o1: expired, o2: reserved, onHand: 2, reserved: 2}, checkO2: true,
		},
		{
			name: "cancel/complete@find", victim: vCancel, at: bFind, by: iCommit,
			first: outStale, retry: outDrop, want: state{o1: committed, onHand: 0, reserved: 0},
		},
		{
			// Version lock for the release path: o2 took spare units meanwhile.
			name: "cancel/spare-units-reserved@save", onHand: 4, victim: vCancel, at: bSave, by: iReserve,
			first: outStale, retry: outOK, want: state{o1: released, o2: reserved, onHand: 4, reserved: 2}, checkO2: true,
		},
		{
			name: "cancel/complete-blocked@update", victim: vCancel, at: bUpdate, by: iCommit,
			first: outOK, want: state{o1: released, onHand: 2, reserved: 0},
			aborted: map[string]int{"inventory.commit_stock": 1},
		},

		// ── expire (sweep) vs complete / cancel ────────────────────────────
		{
			name: "expire/complete@find", victim: vExpire, at: bFind, by: iCommit,
			first: outStale, retry: outOK, retryExpires: 0, want: state{o1: committed, onHand: 0, reserved: 0},
		},
		{
			name: "expire/release@find", victim: vExpire, at: bFind, by: iRelease,
			first: outStale, retry: outOK, retryExpires: 0, want: state{o1: released, onHand: 2, reserved: 0},
		},
		{
			// The sweep must not free the units o2 still holds after o1 sold its own.
			name: "expire/complete-with-o2-hold@find", onHand: 4, holdO2: true, victim: vExpire, at: bFind, by: iCommit,
			first: outStale, retry: outOK, retryExpires: 0, want: state{o1: committed, o2: reserved, onHand: 2, reserved: 2}, checkO2: true,
		},
		{
			name: "expire/spare-units-reserved@save", onHand: 4, victim: vExpire, at: bSave, by: iReserve,
			first: outStale, retry: outOK, retryExpires: 1, want: state{o1: expired, o2: reserved, onHand: 4, reserved: 2}, checkO2: true,
		},
		{
			name: "expire/complete-blocked@update", victim: vExpire, at: bUpdate, by: iCommit,
			first: outOK, want: state{o1: expired, onHand: 2, reserved: 0},
			aborted: map[string]int{"inventory.commit_stock": 1},
		},
	}

	for _, c := range cells {
		t.Run(c.name, func(t *testing.T) {
			if c.onHand == 0 {
				c.onHand = 2
			}
			e := newRaceEnv(t, c.onHand)
			if c.holdO2 {
				e.reserve(e.o2, e.now, dto.Line{SKU: e.sku, Quantity: 2})
			}

			stocks := &hookStocks{Repository: e.stocks}
			reservations := &hookReservations{Repository: e.reservations}
			var parkedErr <-chan error
			fire := func() {
				if err := c.by(e); err != nil {
					t.Errorf("intruder: %v", err)
				}
			}
			switch c.at {
			case bFind:
				stocks.onFind = fire
			case bSave:
				stocks.onSave = fire
			case bUpdate:
				reservations.onUpdate = func(vc context.Context) { parkedErr = e.parked(vc, c.by) }
			}

			msgID := e.p + "-m1"
			if c.victim == vExpire {
				msgID = ""
			}
			err := e.attempt(c.victim, msgID, stocks, reservations)
			if got := classify(err); got != c.first {
				t.Fatalf("first delivery = %v (%s); want %s", err, got, c.first)
			}
			if c.first == outStale {
				var de *kernel.Error
				if errors.As(err, &de) {
					t.Fatalf("first delivery %v carries a kernel.Error %q: the consumer would drop it", err, de.Code())
				}
			}
			if parkedErr != nil {
				// The intruder was released by the victim's commit and must have lost.
				if got := classify(e.join(parkedErr)); got != outStale {
					t.Fatalf("intruder parked on the victim's lock ended %s; want stale", got)
				}
			}

			last := c.first
			if c.first != outOK {
				// Redelivery of the same message: the failed attempt rolled back with
				// its inbox record, so this one must run (not ErrDuplicateMessage).
				err = e.attempt(c.victim, msgID, e.stocks, e.reservations)
				if errors.Is(err, usecasex.ErrDuplicateMessage) {
					t.Fatalf("redelivery was swallowed as a duplicate: the failed attempt's inbox record survived (%v)", err)
				}
				if got := classify(err); got != c.retry {
					t.Fatalf("redelivery = %v (%s); want %s", err, got, c.retry)
				}
				if c.victim == vExpire && e.expiredMine != c.retryExpires {
					t.Fatalf("redelivery expired %d of o1; want %d", e.expiredMine, c.retryExpires)
				}
				last = c.retry
			}

			got := e.read(c.checkO2)
			if got != c.want {
				t.Fatalf("final state = %+v; want %+v", got, c.want)
			}

			// A handler message that was applied is not applied twice.
			if last == outOK && c.victim != vExpire {
				err = e.attempt(c.victim, msgID, e.stocks, e.reservations)
				if !errors.Is(err, usecasex.ErrDuplicateMessage) || classify(err) != outDrop {
					t.Fatalf("third delivery of an applied message = %v; want a dropped duplicate", err)
				}
				if again := e.read(c.checkO2); again != got {
					t.Fatalf("a duplicate delivery moved the state: %+v → %+v", got, again)
				}
			}

			// The rightful holder can still complete: o2's hold is real, not just a number.
			if c.checkO2 && c.want.o2 == reserved {
				out, err := inventory.ProvideCommitStock(e.reservations, e.stocks, e.tx, e.inbox, publisher{}, e.now, e.rec).Execute(e.c, dto.CommitStockInput{OrderID: e.o2})
				if err != nil || out.Status != "COMMITTED" {
					t.Fatalf("o2 commit = %+v, %v; want COMMITTED", out, err)
				}
				if after := e.read(true); after.o2 != committed || after.onHand != got.onHand-2 || after.reserved != got.reserved-2 {
					t.Fatalf("after o2 commit = %+v; want o2 COMMITTED, on_hand %d reserved %d", after, got.onHand-2, got.reserved-2)
				}
			}

			wantAborted := c.aborted
			if wantAborted == nil {
				wantAborted = map[string]int{}
				if c.first == outStale {
					wantAborted[c.victim.usecase()] = 1
				}
			}
			if gotAborted := e.aborted(); fmt.Sprint(gotAborted) != fmt.Sprint(wantAborted) {
				t.Fatalf("aborted use cases = %v; want %v", gotAborted, wantAborted)
			}
		})
	}
}

// Two lines, one of them re-reserved by someone else after the sweep: the late
// commit must end COMMIT_SHORT without moving either SKU (no half sale), and
// the first attempt, which lands on the first line's domain guard, is a
// retryable lost race, not a drop.
func TestCompleteVsExpire_MultiLinePartialShortage(t *testing.T) {
	e := newEnv(t, 2)
	e.setStock(e.skuB, 2)
	e.reserve(e.o1, e.old, dto.Line{SKU: e.sku, Quantity: 2}, dto.Line{SKU: e.skuB, Quantity: 2})

	stocks := &hookStocks{Repository: e.stocks}
	stocks.onFind = func() {
		if err := iExpire(e); err != nil {
			t.Errorf("expire: %v", err)
		}
		// Only line B is taken again; line A stays free.
		if err := e.tryReserve(e.o2, e.now, dto.Line{SKU: e.skuB, Quantity: 2}); err != nil {
			t.Errorf("o2 reserve: %v", err)
		}
	}
	msg := e.p + "-m1"
	if err := e.attempt(vComplete, msg, stocks, e.reservations); classify(err) != outStale {
		t.Fatalf("first delivery = %v; want stale", err)
	}
	if err := e.attempt(vComplete, msg, e.stocks, e.reservations); err != nil {
		t.Fatalf("redelivery: %v", err)
	}

	r, err := e.reservations.FindByOrderID(e.c, e.o1)
	if err != nil || r.Status() != reservation.StatusCommitShort {
		t.Fatalf("o1 = %v, %v; want COMMIT_SHORT", r, err)
	}
	for _, tc := range []struct {
		sku              string
		onHand, reserved int32
	}{{e.sku, 2, 0}, {e.skuB, 2, 2}} { // untouched: A free, B still o2's
		s, err := e.stocks.FindBySKU(e.c, stock.SKU(tc.sku))
		if err != nil || s.OnHand() != tc.onHand || s.Reserved() != tc.reserved {
			t.Fatalf("%s = %v, %v; want on_hand %d reserved %d", tc.sku, s, err, tc.onHand, tc.reserved)
		}
	}
}

// A domain rejection that is not a lost race stays final: the re-read finds the
// reservation exactly as it was loaded, so the error is kept and the message is
// dropped instead of retried forever.
func TestTerminalDomainErrorIsStillDropped(t *testing.T) {
	e := newRaceEnv(t, 2)
	if err := e.attempt(vCancel, "", e.stocks, e.reservations); err != nil {
		t.Fatal(err)
	}

	for _, v := range []victim{vComplete, vCancel} {
		if err := e.attempt(v, "", e.stocks, e.reservations); classify(err) != outDrop {
			t.Fatalf("victim %d over a RELEASED reservation = %v; want a drop", v, err)
		}
	}
	if got := e.read(false); got != (state{o1: reservation.StatusReleased, onHand: 2, reserved: 0}) {
		t.Fatalf("state = %+v", got)
	}
}

var errLookup = errors.New("lookup fault")

// failNthFind fails the n-th FindByOrderID (1-based) with errLookup.
type failNthFind struct {
	reservation.Repository
	n, calls int
}

func (x *failNthFind) FindByOrderID(c context.Context, orderID string) (*reservation.Reservation, error) {
	x.calls++
	if x.calls == x.n {
		return nil, errLookup
	}

	return x.Repository.FindByOrderID(c, orderID)
}

// The re-read that classifies a failure can fail too. Not knowing whether the
// reservation moved is no reason to keep the (final) domain error: through the
// consumer entry point the message must be retried, not dropped, and the same
// message delivered again must then sell.
func TestOrderCompletedHandler_RereadFailureIsRetried(t *testing.T) {
	e := newRaceEnv(t, 2)
	stocks := &hookStocks{Repository: e.stocks}
	stocks.onFind = func() {
		if err := iExpire(e); err != nil {
			t.Errorf("expire: %v", err)
		}
	}
	flaky := &failNthFind{Repository: e.reservations, n: 2} // 1st: the use case's read, 2nd: the re-read
	msg := e.p + "-m1"

	err := e.attempt(vComplete, msg, stocks, flaky)
	if !errors.Is(err, errLookup) || errors.Is(err, consumer.ErrDrop) {
		t.Fatalf("handler = %v, want the lookup error and no ErrDrop", err)
	}
	if err := e.attempt(vComplete, msg, e.stocks, e.reservations); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if got := e.read(false); got != (state{o1: reservation.StatusCommitted, onHand: 0, reserved: 0}) {
		t.Fatalf("state = %+v; want COMMITTED 0/0", got)
	}
}

// The expiry sweep of one cell must not touch another cell's fixture (the
// reviewer's probe: two independently prefixed fixtures, one sweep).
func TestSweepDoesNotTouchOtherFixtures(t *testing.T) {
	a, b := newRaceEnv(t, 2), newRaceEnv(t, 2)

	if err := iExpire(a); err != nil {
		t.Fatal(err)
	}
	if got := a.read(false); got.o1 != reservation.StatusExpired {
		t.Fatalf("a.o1 = %s; want EXPIRED", got.o1)
	}
	if got := b.read(false); got.o1 != reservation.StatusReserved || got.onHand != 2 || got.reserved != 2 {
		t.Fatalf("b was touched by a's sweep: %+v", got)
	}
}

// The @update barrier waits for a session blocked on the victim's backend, not
// for any lock waiter in the database (the reviewer's probe: an unrelated
// advisory-lock wait must not satisfy it).
func TestLockBarrierIgnoresUnrelatedWaiters(t *testing.T) {
	e := newEnv(t, 2)
	const key = 7231
	acquired, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	holderPID, holderDone, err := e.pinned(func(ctx context.Context, conn *gorm.DB) error {
		if err := conn.Exec(`SELECT pg_advisory_lock(?)`, key).Error; err != nil {
			return err
		}
		close(acquired)
		if err := gate(ctx, release); err != nil {
			return err
		}

		return conn.Exec(`SELECT pg_advisory_unlock(?)`, key).Error
	})
	if err != nil {
		t.Fatal(err)
	}
	e.await(acquired, holderDone)

	waiterPID, waiterDone, err := e.pinned(func(ctx context.Context, conn *gorm.DB) error {
		if err := conn.Exec(`SELECT pg_advisory_lock(?)`, key).Error; err != nil {
			return err
		}

		return conn.Exec(`SELECT pg_advisory_unlock(?)`, key).Error
	})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for e.blockedBy(holderPID, waiterPID) == 0 {
		select {
		case err := <-waiterDone:
			t.Fatalf("probe waiter ended (%v) instead of blocking", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("probe waiter never blocked")
		}
		time.Sleep(time.Millisecond)
	}
	// A real lock wait exists in the database, but not on this victim.
	if n := e.blockedBy(e.victimPID(e.c), 0); n != 0 {
		t.Fatalf("blockedBy(unrelated backend) = %d; want 0", n)
	}

	close(release)
	if err := e.join(holderDone); err != nil {
		t.Fatal(err)
	}
	if err := e.join(waiterDone); err != nil {
		t.Fatal(err)
	}
}

// Even a session waiting on the very same victim lock does not count unless it
// is the intended intruder (the reviewer's second probe: an unrelated UPDATE of
// the victim's stock row while the intended intruder is still idle).
func TestLockBarrierRequiresTheIntendedIntruder(t *testing.T) {
	e := newEnv(t, 2)
	update := `UPDATE inventory.stocks SET updated_at = updated_at WHERE sku = ?`
	held, release, releaseIntended := make(chan struct{}), make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		for _, c := range []chan struct{}{release, releaseIntended} {
			select {
			case <-c:
			default:
				close(c)
			}
		}
	})

	vPID, vDone, err := e.pinned(func(ctx context.Context, conn *gorm.DB) error {
		if err := conn.Exec(`BEGIN`).Error; err != nil {
			return err
		}
		if err := conn.Exec(update, e.sku).Error; err != nil { // the victim holds the stock row
			return err
		}
		close(held)
		if err := gate(ctx, release); err != nil {
			return err
		}

		return conn.Exec(`COMMIT`).Error
	})
	if err != nil {
		t.Fatal(err)
	}
	e.await(held, vDone)

	// The intended intruder: connected, not yet at the row.
	iPID, iDone, err := e.pinned(func(ctx context.Context, conn *gorm.DB) error {
		if err := gate(ctx, releaseIntended); err != nil {
			return err
		}

		return conn.Exec(update, e.sku).Error
	})
	if err != nil {
		t.Fatal(err)
	}
	// An unrelated session blocked on the same row.
	uPID, uDone, err := e.pinned(func(ctx context.Context, conn *gorm.DB) error { return conn.Exec(update, e.sku).Error })
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for e.blockedBy(vPID, uPID) == 0 {
		select {
		case err := <-uDone:
			t.Fatalf("unrelated session ended (%v) instead of blocking", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("unrelated waiter never blocked")
		}
		time.Sleep(time.Millisecond)
	}
	if n := e.blockedBy(vPID, iPID); n != 0 {
		t.Fatalf("intended intruder counted as blocked (%d) before it ever reached the row", n)
	}
	if n := e.blockedBy(vPID, 0); n == 0 {
		t.Fatal("sanity: an unrelated waiter on the victim should be visible without the waiter filter")
	}

	close(release)
	close(releaseIntended)
	for _, d := range []<-chan error{vDone, iDone, uDone} {
		if err := e.join(d); err != nil {
			t.Fatal(err)
		}
	}
}

// The intruder's start can fail before its callback runs (closed or exhausted
// pool): that must come back as the real error at once, not as a hang until the
// suite's timeout.
func TestIntruderStartFailureIsReported(t *testing.T) {
	e := newEnv(t, 2)
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(telCleanup)
	dead, cleanup, err := platformpg.NewDB(config.Postgres{URL: pgtest.URL(t)}, tel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	sqlDB, err := dead.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	broken := *e
	broken.db = dead

	ran := false
	started := time.Now()
	pid, done, err := broken.startIntruder(func(*raceEnv) error { ran = true; return nil })
	if err == nil || pid != 0 || done != nil {
		t.Fatalf("startIntruder over a closed pool = %d, %v, %v; want an error", pid, done, err)
	}
	if ran {
		t.Fatal("the intruder ran without a connection")
	}
	if d := time.Since(started); d > 5*time.Second {
		t.Fatalf("start failure took %s to surface; want it prompt", d)
	}
}

// The intruder's actor carries the bounded context into the provider, which
// re-applies it on every statement; an unbounded one would let a stuck intruder
// hang the matrix's join.
func TestIntruderRunsUnderADeadline(t *testing.T) {
	e := newEnv(t, 2)
	_, done, err := e.startIntruder(func(a *raceEnv) error {
		if _, ok := a.c.Deadline(); !ok {
			return errors.New("intruder context has no deadline")
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.join(done); err != nil {
		t.Fatal(err)
	}
}

// A fast actor that starts and finishes before the caller looks is a success,
// not a start failure: with the schedule forced so that both its pid and its
// result are already published when the caller's select runs, the pid decides
// (select alone would pick either at random).
func TestPinnedFastActorIsNotAStartFailure(t *testing.T) {
	e := newEnv(t, 2)
	e.startGate = func(result <-chan error) {
		for deadline := time.Now().Add(15 * time.Second); len(result) == 0; {
			if time.Now().After(deadline) {
				t.Error("actor never finished")

				return
			}
			time.Sleep(time.Millisecond) // wait for an observable state: the published result
		}
	}
	for i := 0; i < 50; i++ {
		pid, done, err := e.pinned(func(context.Context, *gorm.DB) error { return nil })
		if err != nil || pid == 0 {
			t.Fatalf("run %d: pinned = %d, %v; want a started actor", i, pid, err)
		}
		if err := e.join(done); err != nil {
			t.Fatal(err)
		}
	}
}

// The fixture's migration step is bounded by the server, not only by a context
// migrate does not read: with the migration table locked, Up fails at the lock
// timeout instead of waiting forever.
func TestSetupMigrationIsBoundedByServerTimeouts(t *testing.T) {
	e := newEnv(t, 2)
	locked := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	_, holderDone, err := e.pinned(func(ctx context.Context, conn *gorm.DB) error {
		if err := conn.Exec(`BEGIN`).Error; err != nil {
			return err
		}
		if err := conn.Exec(`LOCK TABLE inventory.schema_migrations IN ACCESS EXCLUSIVE MODE`).Error; err != nil {
			return err
		}
		close(locked)
		if err := gate(ctx, release); err != nil {
			return err
		}

		return conn.Exec(`ROLLBACK`).Error
	})
	if err != nil {
		t.Fatal(err)
	}
	e.await(locked, holderDone)

	cfg := config.Postgres{URL: withServerTimeouts(pgtest.URL(t), time.Second, 5*time.Second)}
	started := time.Now()
	if err := platformpg.Up(e.c, cfg, postgres.Migration()); err == nil {
		t.Fatal("Up succeeded while its migration table was locked")
	}
	if d := time.Since(started); d > 20*time.Second {
		t.Fatalf("Up took %s to give up; want the server's lock timeout to bound it", d)
	}

	close(release)
	if err := e.join(holderDone); err != nil {
		t.Fatal(err)
	}
}
