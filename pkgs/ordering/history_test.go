package ordering_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/consumer"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/events"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/metrics"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/rpc"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/ownership"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// ---------------------------------------------------------------------------------------------
// Fixtures: the real providers (decorators, transactor, recorder) over memory or real Postgres.
// Every transition below is driven through a use case, never by calling a repository or the
// history store to make the row appear.
// ---------------------------------------------------------------------------------------------

type hclock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *hclock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *hclock) set(t time.Time) { c.mu.Lock(); defer c.mu.Unlock(); c.t = t }

type hids struct {
	mu     sync.Mutex
	next   string
	issued []string
	n      int
	auto   string
}

func (g *hids) NewID(context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	id := g.next
	if g.auto != "" {
		g.n++
		id = fmt.Sprintf("%s-%d", g.auto, g.n)
	}
	g.issued = append(g.issued, id)

	return id, nil
}

type discardPub struct{}

func (discardPub) Publish(string, ...*message.Message) error { return nil }
func (discardPub) Close() error                              { return nil }

// failingStore fails Append (the nth call, 1-based; 0 = never) and forwards the rest.
type failingStore struct {
	port.HistoryStore
	failOn int32
	calls  atomic.Int32
}

func (s *failingStore) Append(c context.Context, entries ...order.HistoryEntry) error {
	if s.calls.Add(1) == s.failOn {
		return errors.New("injected history failure")
	}

	return s.HistoryStore.Append(c, entries...)
}

type stackOpts struct {
	requirePayment bool
	wrapStore      func(port.HistoryStore) port.HistoryStore
	wrapMsgPub     func(message.Publisher) message.Publisher
	repo           func(order.Repository) order.Repository
}

type hstack struct {
	t       *testing.T
	db      *gorm.DB // nil = memory
	clock   *hclock
	ids     *hids
	repo    order.Repository
	history port.HistoryStore
	tx      port.Transactor
	inbox   port.Inbox
	pub     port.EventPublisher

	create   rpc.CreateOrderUC
	cancel   rpc.CancelOrderUC
	complete rpc.CompleteOrderUC
	confirm  consumer.ConfirmOrderUC
	captured consumer.ApplyPaymentCapturedUC
	expired  consumer.ApplyPaymentExpiredUC
}

const adminRole = "admin"

func newStack(t *testing.T, db *gorm.DB, o stackOpts) *hstack {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	mp := sdkmetric.NewMeterProvider()
	rec, err := usecasex.NewRecorder(mp)
	if err != nil {
		t.Fatal(err)
	}
	createResults, err := metrics.NewCreateOrderResults(mp)
	if err != nil {
		t.Fatal(err)
	}
	paymentResults, err := metrics.NewPaymentEventResults(mp)
	if err != nil {
		t.Fatal(err)
	}

	var msgPub message.Publisher = discardPub{}
	if db != nil {
		pg, err := eventbus.NewPostgresPublisher(db, logger)
		if err != nil {
			t.Fatal(err)
		}
		msgPub = pg
	}
	if o.wrapMsgPub != nil {
		msgPub = o.wrapMsgPub(msgPub)
	}

	s := &hstack{t: t, db: db, clock: &hclock{t: time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)}, ids: &hids{next: "o-1"}}
	s.repo = ordering.ProvideOrderRepository(db, logger)
	if o.repo != nil {
		s.repo = o.repo(s.repo)
	}
	s.tx = ordering.ProvideTransactor(db)
	s.inbox = ordering.ProvideInbox(db)
	s.history = ordering.ProvideHistoryStore(db)
	if o.wrapStore != nil {
		s.history = o.wrapStore(s.history)
	}
	s.pub = ordering.ProvideEventPublisher(events.NewPublisher(msgPub), s.history, config.Auth{AdminRole: adminRole})

	own := ownership.New(adminRole)
	prices := memory.NewFlatPriceList(500, "USD")
	s.create = ordering.ProvideCreateOrder(s.repo, s.tx, s.inbox, s.pub, s.ids, s.clock, own, prices, rec, createResults)
	s.cancel = ordering.ProvideCancelOrder(s.repo, s.tx, s.inbox, s.pub, s.clock, own, rec)
	s.complete = ordering.ProvideCompleteOrder(s.repo, s.tx, s.inbox, s.pub, s.clock, own, rec)
	s.confirm = ordering.ProvideConfirmOrder(config.Ordering{RequirePayment: o.requirePayment}, s.repo, s.tx, s.inbox, s.pub, s.clock, rec)
	s.captured = ordering.ProvideApplyPaymentCaptured(s.repo, s.tx, s.inbox, s.pub, s.clock, rec, paymentResults)
	s.expired = ordering.ProvideApplyPaymentExpired(s.repo, s.tx, s.inbox, s.pub, s.clock, rec, paymentResults)

	return s
}

func asCustomer(sub string) context.Context {
	return contextx.WithPrincipal(context.Background(), contextx.Principal{Subject: sub, Roles: []string{"customer"}})
}
func asAdmin(sub string) context.Context {
	return contextx.WithPrincipal(context.Background(), contextx.Principal{Subject: sub, Roles: []string{adminRole}})
}
func noPrincipal() context.Context { return context.Background() }

func day(h int) time.Time { return time.Date(2026, 11, 4, h, 0, 0, 0, time.UTC) }

// hrow is one history row as the tests compare it: every column, as strings and a UTC instant.
type hrow struct {
	Seq     int
	Event   string
	From    string
	To      string
	At      string
	Actor   string
	Subject string
	Cause   string
}

func want(seq int, event, from, to string, at time.Time, actor, subject, cause string) hrow {
	return hrow{seq, event, from, to, at.UTC().Format(time.RFC3339Nano), actor, subject, cause}
}

func (s *hstack) rows(id string) []hrow {
	s.t.Helper()
	es, err := s.history.List(context.Background(), order.ID(id))
	if err != nil {
		s.t.Fatalf("history of %s: %v", id, err)
	}
	out := make([]hrow, 0, len(es))
	for _, e := range es {
		from := e.From().String()
		if e.From() == order.StatusUnknown {
			from = ""
		}
		out = append(out, hrow{e.Seq(), e.Event(), from, e.To().String(), e.OccurredAt().UTC().Format(time.RFC3339Nano), e.ActorKind().String(), e.ActorSubject(), e.Cause().String()})
	}

	return out
}

func (s *hstack) expectRows(id string, wants ...hrow) {
	s.t.Helper()
	got := s.rows(id)
	if len(wants) == 0 && len(got) == 0 {
		return
	}
	if len(got) != len(wants) {
		s.t.Fatalf("history of %s has %d rows, want %d:\n got  %v\n want %v", id, len(got), len(wants), got, wants)
	}
	for n := range wants {
		if got[n] != wants[n] {
			s.t.Errorf("history of %s row %d:\n got  %+v\n want %+v", id, n+1, got[n], wants[n])
		}
	}
}

func (s *hstack) newOrder(ctx context.Context, id, customer string) {
	s.t.Helper()
	s.ids.next = id
	out, err := s.create.Execute(ctx, dto.CreateOrderInput{CustomerID: customer, Items: []dto.OrderLine{{SKU: "SKU-1", Quantity: 1}}})
	if err != nil || out == nil || out.ID != id {
		s.t.Fatalf("create %s = %+v, %v", id, out, err)
	}
}

func (s *hstack) doConfirm(id string) {
	s.t.Helper()
	if out, err := s.confirm.Execute(noPrincipal(), dto.ConfirmOrderInput{OrderID: id}); err != nil || out == nil || out.Status != "CONFIRMED" {
		s.t.Fatalf("confirm %s = %+v, %v", id, out, err)
	}
}

// backends runs f on memory and (when a database is available) on real Postgres, each with a fresh
// stack factory. Tables are wiped first, so the rows a test sees are its own.
func backends(t *testing.T, f func(t *testing.T, mk func(stackOpts) *hstack)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) {
		f(t, func(o stackOpts) *hstack { return newStack(t, nil, o) })
	})
	t.Run("postgres", func(t *testing.T) {
		db := historyDB(t)
		f(t, func(o stackOpts) *hstack { return newStack(t, db, o) })
	})
}

func historyDB(t *testing.T) *gorm.DB {
	t.Helper()
	c := context.Background()
	cfg := config.Postgres{URL: pgtest.URL(t)}
	tel := pgtest.Telemetry(t)
	db := pgtest.DB(t, cfg, tel)
	if err := platformpg.Up(c, cfg, postgres.Migration()); err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(c).Exec(`TRUNCATE ordering.orders, ordering.order_history,
		ordering.events_v1_ordercreated, ordering.events_v1_orderconfirmed, ordering.events_v1_orderpaid,
		ordering.events_v1_ordercompleted, ordering.events_v1_ordercancelled CASCADE`).Error; err != nil {
		t.Fatal(err)
	}

	return db
}

// ---------------------------------------------------------------------------------------------
// Every transition path writes the row, with the right actor and cause.
// ---------------------------------------------------------------------------------------------

func TestHistory_CreateRecordsActorKinds(t *testing.T) {
	backends(t, func(t *testing.T, mk func(stackOpts) *hstack) {
		s := mk(stackOpts{})
		s.clock.set(day(10))

		s.newOrder(asCustomer("acc-cust"), "o-cust", "acc-cust")
		s.expectRows("o-cust", want(1, "CREATED", "", "PENDING", day(10), "CUSTOMER", "acc-cust", "RPC_REQUEST"))

		// An admin ordering on behalf of a customer: the actor is the admin, not the customer.
		s.newOrder(asAdmin("acc-admin"), "o-admin", "acc-cust")
		s.expectRows("o-admin", want(1, "CREATED", "", "PENDING", day(10), "ADMIN", "acc-admin", "RPC_REQUEST"))

		// auth.mode=off: an rpc call with no principal is ANONYMOUS, not SYSTEM.
		s.newOrder(noPrincipal(), "o-anon", "someone")
		s.expectRows("o-anon", want(1, "CREATED", "", "PENDING", day(10), "ANONYMOUS", "", "RPC_REQUEST"))
	})
}

func TestHistory_ConfirmCompleteCancelPaths(t *testing.T) {
	backends(t, func(t *testing.T, mk func(stackOpts) *hstack) {
		s := mk(stackOpts{})

		// create → confirm (consumer) → complete (rpc, admin)
		s.clock.set(day(10))
		s.newOrder(asCustomer("acc-1"), "o-done", "acc-1")
		s.clock.set(day(11))
		s.doConfirm("o-done")
		s.clock.set(day(12))
		if out, err := s.complete.Execute(asAdmin("acc-admin"), dto.CompleteOrderInput{OrderID: "o-done"}); err != nil || out.Status != "COMPLETED" {
			t.Fatalf("complete = %+v, %v", out, err)
		}
		s.expectRows("o-done",
			want(1, "CREATED", "", "PENDING", day(10), "CUSTOMER", "acc-1", "RPC_REQUEST"),
			want(2, "CONFIRMED", "PENDING", "CONFIRMED", day(11), "SYSTEM", "", "STOCK_RESERVED"),
			want(3, "COMPLETED", "CONFIRMED", "COMPLETED", day(12), "ADMIN", "acc-admin", "RPC_REQUEST"),
		)

		// cancel by the customer through rpc, from PENDING
		s.clock.set(day(10))
		s.newOrder(asCustomer("acc-2"), "o-rpc-cancel", "acc-2")
		s.clock.set(day(13))
		if out, err := s.cancel.Execute(asCustomer("acc-2"), dto.CancelOrderInput{OrderID: "o-rpc-cancel", Source: dto.CancelSourceRPC}); err != nil || out.Status != "CANCELLED" {
			t.Fatalf("rpc cancel = %+v, %v", out, err)
		}
		s.expectRows("o-rpc-cancel",
			want(1, "CREATED", "", "PENDING", day(10), "CUSTOMER", "acc-2", "RPC_REQUEST"),
			want(2, "CANCELLED", "PENDING", "CANCELLED", day(13), "CUSTOMER", "acc-2", "RPC_REQUEST"),
		)

		// cancel requested by another BC (consumer path, no principal), from CONFIRMED. The upstream
		// reason text must not appear in any column.
		const reason = "reservation r-77 for SKU-SECRET expired after 24h0m0s"
		s.clock.set(day(10))
		s.newOrder(asCustomer("acc-3"), "o-req-cancel", "acc-3")
		s.clock.set(day(11))
		s.doConfirm("o-req-cancel")
		s.clock.set(day(14))
		if out, err := s.cancel.Execute(noPrincipal(), dto.CancelOrderInput{OrderID: "o-req-cancel", Source: dto.CancelSourceCancellationRequest, Reason: reason}); err != nil || out.Status != "CANCELLED" {
			t.Fatalf("requested cancel = %+v, %v", out, err)
		}
		s.expectRows("o-req-cancel",
			want(1, "CREATED", "", "PENDING", day(10), "CUSTOMER", "acc-3", "RPC_REQUEST"),
			want(2, "CONFIRMED", "PENDING", "CONFIRMED", day(11), "SYSTEM", "", "STOCK_RESERVED"),
			want(3, "CANCELLED", "CONFIRMED", "CANCELLED", day(14), "SYSTEM", "", "CANCELLATION_REQUESTED"),
		)
		for _, r := range s.rows("o-req-cancel") {
			if strings.Contains(fmt.Sprintf("%+v", r), "SECRET") || strings.Contains(fmt.Sprintf("%+v", r), "24h") {
				t.Fatalf("upstream reason leaked into history: %+v", r)
			}
		}
	})
}

// A cancel whose source nobody said is refused, not defaulted to "the customer asked".
func TestHistory_CancelWithoutSourceIsRefused(t *testing.T) {
	backends(t, func(t *testing.T, mk func(stackOpts) *hstack) {
		s := mk(stackOpts{})
		s.clock.set(day(10))
		s.newOrder(asCustomer("acc-1"), "o-nosrc", "acc-1")
		before := s.rows("o-nosrc")
		if len(before) != 1 {
			t.Fatalf("setup: %d rows, want 1", len(before))
		}

		out, err := s.cancel.Execute(asCustomer("acc-1"), dto.CancelOrderInput{OrderID: "o-nosrc"})
		if !errors.Is(err, order.ErrInvalidCause) || out != nil {
			t.Fatalf("cancel without source = %+v, %v; want ErrInvalidCause", out, err)
		}
		if got, err := s.repo.FindByID(context.Background(), "o-nosrc"); err != nil || got.Status() != order.StatusPending {
			t.Fatalf("order after the refused cancel = %v, %v; want PENDING", got, err)
		}
		s.expectRows("o-nosrc", before...)
	})
}

// ---------------------------------------------------------------------------------------------
// payment_required matrix: on / off / the default. PaymentCaptured and PaymentExpired both.
// ---------------------------------------------------------------------------------------------

func TestHistory_PaymentRequiredMatrix(t *testing.T) {
	loaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Ordering.RequirePayment {
		t.Skip("the environment turns ordering.require_payment on; the default cell cannot be exercised")
	}

	cells := []struct {
		name string
		cfg  config.Ordering // what ProvideConfirmOrder is given
		paid bool            // whether the order's snapshot says payment is owed
	}{
		{"on", config.Ordering{RequirePayment: true}, true},
		{"off", config.Ordering{RequirePayment: false}, false},
		{"default (config.Load)", loaded.Ordering, false},
		{"zero value", config.Ordering{}, false},
	}
	for _, cell := range cells {
		t.Run(cell.name, func(t *testing.T) {
			backends(t, func(t *testing.T, mk func(stackOpts) *hstack) {
				s := mk(stackOpts{requirePayment: cell.cfg.RequirePayment})

				seed := func(id string) {
					s.clock.set(day(10))
					s.newOrder(asCustomer("acc-1"), id, "acc-1")
					s.clock.set(day(11))
					s.doConfirm(id)
				}

				// PaymentCaptured
				seed("o-cap")
				s.clock.set(day(12))
				out, err := s.captured.Execute(noPrincipal(), dto.PaymentCapturedInput{OrderID: "o-cap"})
				if err != nil || out == nil {
					t.Fatalf("captured = %+v, %v", out, err)
				}
				base := []hrow{
					want(1, "CREATED", "", "PENDING", day(10), "CUSTOMER", "acc-1", "RPC_REQUEST"),
					want(2, "CONFIRMED", "PENDING", "CONFIRMED", day(11), "SYSTEM", "", "STOCK_RESERVED"),
				}
				if cell.paid {
					s.expectRows("o-cap", append(base,
						want(3, "PAID", "CONFIRMED", "PAID", day(12), "SYSTEM", "", "PAYMENT_CAPTURED"),
						want(4, "COMPLETED", "PAID", "COMPLETED", day(12), "SYSTEM", "", "PAYMENT_CAPTURED"),
					)...)
					// A redelivery is a no-op: no fifth row. Expired after completion is refused: still four.
					s.clock.set(day(13))
					if _, err := s.captured.Execute(noPrincipal(), dto.PaymentCapturedInput{OrderID: "o-cap"}); err != nil {
						t.Fatalf("redelivered capture: %v", err)
					}
					if _, err := s.expired.Execute(noPrincipal(), dto.PaymentExpiredInput{OrderID: "o-cap"}); !errors.Is(err, order.ErrCannotCancel) {
						t.Fatalf("expired after capture err = %v, want ErrCannotCancel", err)
					}
					if n := len(s.rows("o-cap")); n != 4 {
						t.Fatalf("history has %d rows after redelivery and a refused expiry, want 4", n)
					}
				} else {
					// The payment event does nothing to an order that owes none: no PAID row, ever.
					s.expectRows("o-cap", base...)
					s.clock.set(day(13))
					if _, err := s.complete.Execute(asAdmin("acc-admin"), dto.CompleteOrderInput{OrderID: "o-cap"}); err != nil {
						t.Fatalf("complete: %v", err)
					}
					s.expectRows("o-cap", append(base, want(3, "COMPLETED", "CONFIRMED", "COMPLETED", day(13), "ADMIN", "acc-admin", "RPC_REQUEST"))...)
				}

				// PaymentExpired
				seed("o-exp")
				s.clock.set(day(12))
				if _, err := s.expired.Execute(noPrincipal(), dto.PaymentExpiredInput{OrderID: "o-exp"}); err != nil {
					t.Fatalf("expired: %v", err)
				}
				if cell.paid {
					s.expectRows("o-exp", append(base,
						want(3, "CANCELLED", "CONFIRMED", "CANCELLED", day(12), "SYSTEM", "", "PAYMENT_EXPIRED"))...)
				} else {
					s.expectRows("o-exp", base...)
				}
			})
		})
	}
}

// ---------------------------------------------------------------------------------------------
// Paths that publish nothing write nothing.
// ---------------------------------------------------------------------------------------------

func TestHistory_ReplayAndKeyConflictWriteNothing(t *testing.T) {
	backends(t, func(t *testing.T, mk func(stackOpts) *hstack) {
		s := mk(stackOpts{})
		s.clock.set(day(10))
		ctx := asCustomer("acc-1")
		in := dto.CreateOrderInput{Items: []dto.OrderLine{{SKU: "SKU-1", Quantity: 1}}, IdempotencyKey: "k-1"}

		s.ids.next = "o-first"
		first, err := s.create.Execute(ctx, in)
		if err != nil || first == nil {
			t.Fatalf("first = %+v, %v", first, err)
		}
		wantFirst := want(1, "CREATED", "", "PENDING", day(10), "CUSTOMER", "acc-1", "RPC_REQUEST")
		s.expectRows("o-first", wantFirst)

		// Replay of the same request: the original order comes back, no event, no row.
		s.ids.next = "o-replay"
		again, err := s.create.Execute(ctx, in)
		if err != nil || again == nil || again.ID != "o-first" {
			t.Fatalf("replay = %+v, %v; want the first order", again, err)
		}
		s.expectRows("o-first", wantFirst)
		s.expectRows("o-replay") // the id the replay would have used never got a row

		// Same key, different request: refused, still nothing.
		s.ids.next = "o-reuse"
		if _, err := s.create.Execute(ctx, dto.CreateOrderInput{Items: []dto.OrderLine{{SKU: "SKU-2", Quantity: 1}}, IdempotencyKey: "k-1"}); !errors.Is(err, order.ErrIdempotencyKeyReused) {
			t.Fatalf("reuse err = %v", err)
		}
		s.expectRows("o-reuse")
		s.expectRows("o-first", wantFirst)
	})
}

// ErrIdempotencyConflict is returned by Save before Publish: concurrent first uses of one key leave
// exactly one order and exactly one history row; every id a loser generated has none.
func TestHistory_IdempotencyConflictRaceWritesOneRow(t *testing.T) {
	backends(t, func(t *testing.T, mk func(stackOpts) *hstack) {
		s := mk(stackOpts{})
		s.ids.auto = "o-race"
		s.clock.set(day(10))
		ctx := asCustomer("acc-1")
		in := dto.CreateOrderInput{Items: []dto.OrderLine{{SKU: "SKU-1", Quantity: 1}}, IdempotencyKey: "k-race"}

		var wg sync.WaitGroup
		start := make(chan struct{})
		winners := make(chan string, 8)
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				out, err := s.create.Execute(ctx, in)
				if err != nil || out == nil {
					t.Errorf("create: %v", err)
					return
				}
				winners <- out.ID
			}()
		}
		close(start)
		wg.Wait()
		close(winners)

		seen := map[string]bool{}
		for id := range winners {
			seen[id] = true
		}
		if len(seen) != 1 {
			t.Fatalf("callers got %d distinct orders %v, want 1", len(seen), seen)
		}
		withRows := 0
		for _, id := range s.ids.issued {
			rows := s.rows(id)
			if len(rows) > 0 {
				withRows++
				if len(rows) != 1 || rows[0].Seq != 1 || rows[0].Event != "CREATED" {
					t.Errorf("history of the winner %s = %+v", id, rows)
				}
			}
		}
		if withRows != 1 {
			t.Fatalf("%d of %d generated ids have history, want exactly 1 (losers return before Publish)", withRows, len(s.ids.issued))
		}
	})
}

// blindSecondLookup makes the 2nd FindByIdempotencyKey answer "not found" although the key is taken:
// the window in which two first uses of one key both see nothing. The 2nd request then generates an
// id, loses at Save (ErrIdempotencyConflict) and reads the winner back.
type blindSecondLookup struct {
	order.Repository
	n       atomic.Int32
	blinded atomic.Bool
}

func (r *blindSecondLookup) FindByIdempotencyKey(c context.Context, customer, key string) (*order.Order, error) {
	if r.n.Add(1) == 2 {
		r.blinded.Store(true)

		return nil, order.ErrNotFound
	}

	return r.Repository.FindByIdempotencyKey(c, customer, key)
}

// The deterministic form of the race above: the loser reaches Save, gets ErrIdempotencyConflict
// (before Publish) and returns the winner's order. It left no outbox row and no history row.
func TestHistory_IdempotencyConflictWritesNothing(t *testing.T) {
	backends(t, func(t *testing.T, mk func(stackOpts) *hstack) {
		var blind *blindSecondLookup
		s := mk(stackOpts{repo: func(r order.Repository) order.Repository { blind = &blindSecondLookup{Repository: r}; return blind }})
		s.clock.set(day(10))
		ctx := asCustomer("acc-1")
		in := dto.CreateOrderInput{Items: []dto.OrderLine{{SKU: "SKU-1", Quantity: 1}}, IdempotencyKey: "k-conflict"}

		s.ids.next = "o-winner"
		if out, err := s.create.Execute(ctx, in); err != nil || out.ID != "o-winner" {
			t.Fatalf("winner = %+v, %v", out, err)
		}
		s.ids.next = "o-loser"
		out, err := s.create.Execute(ctx, in)
		if err != nil || out == nil || out.ID != "o-winner" {
			t.Fatalf("loser = %+v, %v; want the winner's order", out, err)
		}
		if !blind.blinded.Load() {
			t.Fatal("the lookup was never blinded: the conflict path was not exercised")
		}
		if len(s.ids.issued) != 2 {
			t.Fatalf("ids issued = %v, want both requests to have generated one", s.ids.issued)
		}
		s.expectRows("o-winner", want(1, "CREATED", "", "PENDING", day(10), "CUSTOMER", "acc-1", "RPC_REQUEST"))
		s.expectRows("o-loser")
		if s.db != nil {
			var n int64
			if err := s.db.Raw(`SELECT count(*) FROM ordering.events_v1_ordercreated`).Scan(&n).Error; err != nil || n != 1 {
				t.Fatalf("outbox holds %d OrderCreated, %v; want 1", n, err)
			}
		}
	})
}

// An order that existed before the history table came: no rows, no error; the first transition
// after that starts at seq 1 and the earlier part is simply missing.
func TestHistory_ExistingOrderWithoutHistory(t *testing.T) {
	backends(t, func(t *testing.T, mk func(stackOpts) *hstack) {
		s := mk(stackOpts{})
		s.clock.set(day(9))

		// Fixture: an order that was CONFIRMED before this migration, saved without any history.
		seed, err := order.Reconstitute(order.ReconstituteParams{
			ID: "o-legacy", CustomerID: "acc-1", Status: order.StatusConfirmed, CreatedAt: day(8), UpdatedAt: day(8),
			Items: legacyItems(t),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.repo.Save(context.Background(), seed); err != nil {
			t.Fatal(err)
		}

		got, err := s.history.List(context.Background(), "o-legacy")
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("List of an order with no history = %v (nil=%v), %v; want an empty, non-nil list and no error", got, got == nil, err)
		}
		// Control: an id that does not exist behaves the same — the store does not know orders.
		if got, err := s.history.List(context.Background(), "o-never"); err != nil || len(got) != 0 {
			t.Fatalf("List of an unknown order = %v, %v", got, err)
		}

		s.clock.set(day(12))
		if out, err := s.cancel.Execute(asAdmin("acc-admin"), dto.CancelOrderInput{OrderID: "o-legacy", Source: dto.CancelSourceRPC}); err != nil || out.Status != "CANCELLED" {
			t.Fatalf("cancel = %+v, %v", out, err)
		}
		s.expectRows("o-legacy", want(1, "CANCELLED", "CONFIRMED", "CANCELLED", day(12), "ADMIN", "acc-admin", "RPC_REQUEST"))
	})
}

// ---------------------------------------------------------------------------------------------
// Atomicity on real Postgres: history and outbox commit or roll back together, both directions,
// plus a control that proves the assertions go red when nothing makes them atomic.
// ---------------------------------------------------------------------------------------------

type atomicitySnapshot struct {
	status  string
	version int64
	paid    int64
	done    int64
	history int64
}

func (s *hstack) snapshot(id string) atomicitySnapshot {
	s.t.Helper()
	o, err := s.repo.FindByID(context.Background(), order.ID(id))
	if err != nil || o == nil {
		s.t.Fatalf("snapshot: find %s = %v, %v", id, o, err)
	}
	snap := atomicitySnapshot{status: o.Status().String(), version: o.Version()}
	count := func(q string, args ...any) int64 {
		var n int64
		if err := s.db.Raw(q, args...).Scan(&n).Error; err != nil {
			s.t.Fatal(err)
		}

		return n
	}
	snap.paid = count(`SELECT count(*) FROM ordering.events_v1_orderpaid`)
	snap.done = count(`SELECT count(*) FROM ordering.events_v1_ordercompleted`)
	snap.history = count(`SELECT count(*) FROM ordering.order_history WHERE order_id = ?`, id)

	return snap
}

func TestHistory_AtomicWithOutbox(t *testing.T) {
	db := historyDB(t)
	topicCompleted := events.TopicOrderCompleted

	// seedConfirmed leaves o-atomic CONFIRMED with a payment owed, through the real use cases.
	seedConfirmed := func(s *hstack) {
		s.clock.set(day(10))
		s.newOrder(asCustomer("acc-1"), "o-atomic", "acc-1")
		s.clock.set(day(11))
		s.doConfirm("o-atomic")
	}
	capture := func(s *hstack) error {
		s.clock.set(day(12))
		_, err := s.captured.Execute(noPrincipal(), dto.PaymentCapturedInput{OrderID: "o-atomic"})

		return err
	}
	reset := func() {
		if err := db.Exec(`TRUNCATE ordering.orders, ordering.order_history, ordering.events_v1_ordercreated, ordering.events_v1_orderconfirmed,
			ordering.events_v1_orderpaid, ordering.events_v1_ordercompleted, ordering.events_v1_ordercancelled CASCADE`).Error; err != nil {
			t.Fatal(err)
		}
	}

	t.Run("positive control: without a fault the capture writes order, outbox and history", func(t *testing.T) {
		reset()
		s := newStack(t, db, stackOpts{requirePayment: true})
		seedConfirmed(s)
		before := s.snapshot("o-atomic")
		if before.status != "CONFIRMED" || before.history != 2 || before.paid != 0 || before.done != 0 {
			t.Fatalf("setup snapshot = %+v", before)
		}
		if err := capture(s); err != nil {
			t.Fatal(err)
		}
		after := s.snapshot("o-atomic")
		if after.status != "COMPLETED" || after.version <= before.version || after.paid != 1 || after.done != 1 || after.history != 4 {
			t.Fatalf("after snapshot = %+v (before %+v)", after, before)
		}
	})

	t.Run("history write fails: order and outbox roll back", func(t *testing.T) {
		reset()
		fs := &failingStore{failOn: 3} // creates +1, confirm +1, so the 3rd Append is the capture's first (PAID)
		s := newStack(t, db, stackOpts{requirePayment: true, wrapStore: func(h port.HistoryStore) port.HistoryStore { fs.HistoryStore = h; return fs }})
		seedConfirmed(s)
		before := s.snapshot("o-atomic")
		if before.status != "CONFIRMED" || before.history != 2 {
			t.Fatalf("setup snapshot = %+v", before)
		}
		if err := capture(s); err == nil || !strings.Contains(err.Error(), "injected history failure") {
			t.Fatalf("capture err = %v, want the injected history failure", err)
		}
		if fs.calls.Load() != 3 {
			t.Fatalf("history Append called %d times, want 3 (the fault was never reached)", fs.calls.Load())
		}
		if after := s.snapshot("o-atomic"); after != before {
			t.Fatalf("snapshot after the failed capture = %+v, want unchanged %+v", after, before)
		}
	})

	t.Run("outbox write fails after the first history row: history rolls back", func(t *testing.T) {
		reset()
		var tg *toggledFail
		s := newStack(t, db, stackOpts{requirePayment: true, wrapMsgPub: func(p message.Publisher) message.Publisher {
			tg = &toggledFail{inner: p, topic: topicCompleted}
			return tg
		}})
		seedConfirmed(s)
		tg.fail.Store(true) // after the seed: creating and confirming do not touch OrderCompleted
		before := s.snapshot("o-atomic")
		if before.status != "CONFIRMED" || before.history != 2 {
			t.Fatalf("setup snapshot = %+v", before)
		}
		if err := capture(s); err == nil || !strings.Contains(err.Error(), "injected outbox failure") {
			t.Fatalf("capture err = %v, want the injected outbox failure", err)
		}
		if after := s.snapshot("o-atomic"); after != before {
			t.Fatalf("snapshot after the failed capture = %+v, want unchanged %+v (PAID's history and outbox row must not survive)", after, before)
		}
	})

	// The same fault with no transaction around the use case: the PAID history row is left behind. If
	// this control ever passes the rollback assertions above, they were vacuous.
	t.Run("control: without Transactional the same fault leaves the PAID row behind", func(t *testing.T) {
		reset()
		var tg *toggledFail
		s := newStack(t, db, stackOpts{requirePayment: true, wrapMsgPub: func(p message.Publisher) message.Publisher {
			tg = &toggledFail{inner: p, topic: topicCompleted}
			return tg
		}})
		seedConfirmed(s)
		tg.fail.Store(true)
		before := s.snapshot("o-atomic")
		bare := usecase.NewApplyPaymentCaptured(s.repo, s.pub, s.clock)
		if _, err := bare.Execute(noPrincipal(), dto.PaymentCapturedInput{OrderID: "o-atomic"}); err == nil {
			t.Fatal("the bare use case did not fail")
		}
		after := s.snapshot("o-atomic")
		if after.history == before.history && after.paid == before.paid && after.status == before.status {
			t.Fatalf("snapshot unchanged (%+v) with no transaction: the rollback assertions above cannot tell atomic from non-atomic", after)
		}
	})
}

// toggledFail fails Publish for one topic once switched on.
type toggledFail struct {
	inner message.Publisher
	topic string
	fail  atomic.Bool
}

func (p *toggledFail) Publish(topic string, msgs ...*message.Message) error {
	if topic == p.topic && p.fail.Load() {
		return errors.New("injected outbox failure")
	}

	return p.inner.Publish(topic, msgs...)
}
func (p *toggledFail) Close() error { return p.inner.Close() }

// ---------------------------------------------------------------------------------------------
// Concurrency on real Postgres: two writers on one order. Save's row lock serialises them, the loser
// is stale, reloads and finds nothing to do; seq stays 1..N with no gap and no duplicate.
// ---------------------------------------------------------------------------------------------

type staleProbe struct {
	order.Repository
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	finds   atomic.Int32
	stale   atomic.Int32
}

func (g *staleProbe) FindByID(c context.Context, id order.ID) (*order.Order, error) {
	g.finds.Add(1)

	return g.Repository.FindByID(c, id)
}

func (g *staleProbe) Save(c context.Context, o *order.Order) error {
	g.once.Do(func() {
		close(g.entered)
		<-g.release
	})
	err := g.Repository.Save(c, o)
	if errors.Is(err, order.ErrStale) {
		g.stale.Add(1)
	}

	return err
}

func TestHistory_ConcurrentWritersSerialise(t *testing.T) {
	db := historyDB(t)
	seedStack := newStack(t, db, stackOpts{requirePayment: true})
	seedStack.clock.set(day(10))
	seedStack.newOrder(asCustomer("acc-1"), "o-conc", "acc-1")
	seedStack.clock.set(day(11))
	seedStack.doConfirm("o-conc")

	probe := &staleProbe{entered: make(chan struct{}), release: make(chan struct{})}
	loser := newStack(t, db, stackOpts{requirePayment: true, repo: func(r order.Repository) order.Repository { probe.Repository = r; return probe }})
	loser.clock.set(day(12))
	seedStack.clock.set(day(12))

	loserDone := make(chan error, 1)
	go func() { // fire-and-forget: joined through loserDone below
		_, err := loser.captured.Execute(noPrincipal(), dto.PaymentCapturedInput{OrderID: "o-conc"})
		loserDone <- err
	}()
	select {
	case <-probe.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the loser never reached its Save")
	}
	if _, err := seedStack.captured.Execute(noPrincipal(), dto.PaymentCapturedInput{OrderID: "o-conc"}); err != nil {
		t.Fatalf("winner: %v", err)
	}
	close(probe.release)
	select {
	case err := <-loserDone:
		if err != nil {
			t.Fatalf("loser after its retry = %v, want success (a no-op)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the loser never finished")
	}

	if probe.stale.Load() != 1 || probe.finds.Load() != 2 {
		t.Fatalf("loser saw %d stale saves and %d loads, want 1 and 2 (stale once, then a retry)", probe.stale.Load(), probe.finds.Load())
	}
	seedStack.expectRows("o-conc",
		want(1, "CREATED", "", "PENDING", day(10), "CUSTOMER", "acc-1", "RPC_REQUEST"),
		want(2, "CONFIRMED", "PENDING", "CONFIRMED", day(11), "SYSTEM", "", "STOCK_RESERVED"),
		want(3, "PAID", "CONFIRMED", "PAID", day(12), "SYSTEM", "", "PAYMENT_CAPTURED"),
		want(4, "COMPLETED", "PAID", "COMPLETED", day(12), "SYSTEM", "", "PAYMENT_CAPTURED"),
	)
}

func legacyItems(t *testing.T) []order.LineItem {
	t.Helper()
	usd, err := kernel.NewMoney(500, "USD")
	if err != nil {
		t.Fatal(err)
	}
	it, err := order.NewLineItem("SKU-1", 1, usd)
	if err != nil {
		t.Fatal(err)
	}

	return []order.LineItem{it}
}
