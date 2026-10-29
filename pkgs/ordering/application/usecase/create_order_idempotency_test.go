package usecase_test

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

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/events"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/ownership"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// idemSeq is process-wide: subtests share one database, so ids must never repeat.
var idemSeq atomic.Int64

// atomicIDs is safe for the concurrent tests (seqIDs is not).
type atomicIDs struct{}

func (atomicIDs) NewID(context.Context) (string, error) {
	return fmt.Sprintf("idem-%03d", idemSeq.Add(1)), nil
}

// lockedRecorder is a port.EventPublisher safe for concurrent Execute calls.
type lockedRecorder struct {
	mu     sync.Mutex
	events []order.Event
}

func (r *lockedRecorder) Publish(_ context.Context, evs ...order.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, evs...)

	return nil
}

func (r *lockedRecorder) created() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if _, ok := e.(order.Created); ok {
			n++
		}
	}

	return n
}

func items(skus ...string) []dto.LineItem {
	out := make([]dto.LineItem, 0, len(skus))
	for _, s := range skus {
		out = append(out, dto.LineItem{SKU: s, Quantity: 1, UnitPrice: dto.Money{AmountMinor: 500, Currency: "USD"}})
	}

	return out
}

type idemFixture struct {
	create *usecase.CreateOrder
	list   *usecase.ListOrders
	events *lockedRecorder
	// base is how many orders the shared database already held: subtests count only their own.
	base int
}

func newIdemFixture(repo order.Repository) idemFixture {
	events := &lockedRecorder{}
	clock := fixedClock{t: time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)}
	own := ownership.New(adminRole)

	f := idemFixture{
		create: usecase.NewCreateOrder(repo, events, atomicIDs{}, clock, own),
		list:   usecase.NewListOrders(repo, own),
		events: events,
	}
	f.base = f.count()

	return f
}

func (f idemFixture) count() int {
	out, err := f.list.Execute(as(ops), dto.ListOrdersInput{})
	if err != nil {
		panic(err)
	}

	return len(out.Orders)
}

// total is the number of orders this subtest created.
func (f idemFixture) total(t *testing.T, _ context.Context) int {
	t.Helper()

	return f.count() - f.base
}

func TestCreateOrder_IdempotencyKey(t *testing.T) {
	repos(t, func(t *testing.T, repo order.Repository) {
		t.Run("same key same items replays the original order", func(t *testing.T) {
			f := newIdemFixture(repo)
			in := dto.CreateOrderInput{Items: items("SKU-1", "SKU-2"), IdempotencyKey: "k-replay"}

			first, err := f.create.Execute(as(alice), in)
			if err != nil {
				t.Fatalf("first: %v", err)
			}
			second, err := f.create.Execute(as(alice), in)
			if err != nil {
				t.Fatalf("second: %v", err)
			}

			if second.ID != first.ID {
				t.Fatalf("replay created %s, want the original %s", second.ID, first.ID)
			}
			if first.Replayed || !second.Replayed {
				t.Fatalf("Replayed = %v / %v, want false for the create and true for the replay", first.Replayed, second.Replayed)
			}
			if n := f.total(t, as(ops)); n != 1 {
				t.Fatalf("%d orders stored, want 1", n)
			}
			if n := f.events.created(); n != 1 {
				t.Fatalf("%d OrderCreated published, want 1", n)
			}
		})

		t.Run("no key keeps creating a new order every time", func(t *testing.T) {
			f := newIdemFixture(repo)
			in := dto.CreateOrderInput{Items: items("SKU-1")}

			a, err := f.create.Execute(as(alice), in)
			if err != nil {
				t.Fatal(err)
			}
			b, err := f.create.Execute(as(alice), in)
			if err != nil {
				t.Fatal(err)
			}

			if a.ID == b.ID {
				t.Fatalf("keyless requests collapsed into %s", a.ID)
			}
			if n := f.events.created(); n != 2 {
				t.Fatalf("%d OrderCreated published, want 2", n)
			}
		})

		t.Run("same key different items is key reuse and writes nothing", func(t *testing.T) {
			f := newIdemFixture(repo)
			orig, err := f.create.Execute(as(alice), dto.CreateOrderInput{Items: items("SKU-1"), IdempotencyKey: "k-reuse"})
			if err != nil {
				t.Fatal(err)
			}

			_, err = f.create.Execute(as(alice), dto.CreateOrderInput{Items: items("SKU-SECRET"), IdempotencyKey: "k-reuse"})
			if !errors.Is(err, order.ErrIdempotencyKeyReused) {
				t.Fatalf("err = %v, want ErrIdempotencyKeyReused", err)
			}
			for _, leak := range []string{orig.ID, "SKU-1", "SKU-SECRET", "k-reuse", "acct-alice"} {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("error %q leaks %q", err, leak)
				}
			}
			if n := f.total(t, as(ops)); n != 1 {
				t.Fatalf("%d orders stored, want 1", n)
			}
			if n := f.events.created(); n != 1 {
				t.Fatalf("%d OrderCreated published, want 1", n)
			}
		})

		t.Run("item order is part of the request", func(t *testing.T) {
			f := newIdemFixture(repo)
			if _, err := f.create.Execute(as(alice), dto.CreateOrderInput{Items: items("SKU-1", "SKU-2"), IdempotencyKey: "k-order"}); err != nil {
				t.Fatal(err)
			}

			_, err := f.create.Execute(as(alice), dto.CreateOrderInput{Items: items("SKU-2", "SKU-1"), IdempotencyKey: "k-order"})
			if !errors.Is(err, order.ErrIdempotencyKeyReused) {
				t.Fatalf("reordered items err = %v, want ErrIdempotencyKeyReused", err)
			}
		})

		t.Run("quantity and price are part of the request", func(t *testing.T) {
			f := newIdemFixture(repo)
			base := dto.CreateOrderInput{Items: items("SKU-1"), IdempotencyKey: "k-fields"}
			if _, err := f.create.Execute(as(alice), base); err != nil {
				t.Fatal(err)
			}

			qty := dto.CreateOrderInput{Items: items("SKU-1"), IdempotencyKey: "k-fields"}
			qty.Items[0].Quantity = 2
			price := dto.CreateOrderInput{Items: items("SKU-1"), IdempotencyKey: "k-fields"}
			price.Items[0].UnitPrice.AmountMinor = 501
			for name, in := range map[string]dto.CreateOrderInput{"quantity": qty, "price": price} {
				if _, err := f.create.Execute(as(alice), in); !errors.Is(err, order.ErrIdempotencyKeyReused) {
					t.Fatalf("%s change err = %v, want ErrIdempotencyKeyReused", name, err)
				}
			}
		})

		t.Run("the key is scoped to the customer", func(t *testing.T) {
			f := newIdemFixture(repo)
			in := dto.CreateOrderInput{Items: items("SKU-1"), IdempotencyKey: "k-shared"}

			a, err := f.create.Execute(as(alice), in)
			if err != nil {
				t.Fatal(err)
			}
			// Bob spending the same key (even with other items) is a different customer: no collision, no peek.
			b, err := f.create.Execute(as(bob), dto.CreateOrderInput{Items: items("SKU-9"), IdempotencyKey: "k-shared"})
			if err != nil {
				t.Fatalf("bob: %v", err)
			}
			if a.ID == b.ID || b.CustomerID != "acct-bob" {
				t.Fatalf("alice %+v, bob %+v: want two separate orders", a, b)
			}

			// An admin acting for alice lands in alice's scope and replays her order.
			onBehalf := in
			onBehalf.CustomerID = "acct-alice"
			r, err := f.create.Execute(as(ops), onBehalf)
			if err != nil || r.ID != a.ID {
				t.Fatalf("admin for alice = %+v, %v; want replay of %s", r, err, a.ID)
			}
			// ...and for bob lands in bob's scope.
			onBehalf = dto.CreateOrderInput{CustomerID: "acct-bob", Items: items("SKU-9"), IdempotencyKey: "k-shared"}
			r, err = f.create.Execute(as(ops), onBehalf)
			if err != nil || r.ID != b.ID {
				t.Fatalf("admin for bob = %+v, %v; want replay of %s", r, err, b.ID)
			}
		})

		t.Run("replay returns the order as it is now", func(t *testing.T) {
			f := newIdemFixture(repo)
			in := dto.CreateOrderInput{Items: items("SKU-1"), IdempotencyKey: "k-now"}
			first, err := f.create.Execute(as(alice), in)
			if err != nil {
				t.Fatal(err)
			}
			confirm := usecase.NewConfirmOrder(repo, &lockedRecorder{}, fixedClock{t: time.Date(2026, 10, 29, 9, 5, 0, 0, time.UTC)})
			if _, err := confirm.Execute(context.Background(), dto.ConfirmOrderInput{OrderID: first.ID}); err != nil {
				t.Fatal(err)
			}

			again, err := f.create.Execute(as(alice), in)
			if err != nil {
				t.Fatal(err)
			}
			if again.ID != first.ID || again.Status != "CONFIRMED" {
				t.Fatalf("replay = %s %s, want %s CONFIRMED", again.ID, again.Status, first.ID)
			}
		})

		t.Run("concurrent same key creates exactly one order", func(t *testing.T) {
			f := newIdemFixture(repo)
			in := dto.CreateOrderInput{Items: items("SKU-1"), IdempotencyKey: "k-race"}

			const n = 8
			ids := make([]string, n)
			replayed := make([]bool, n)
			errs := make([]error, n)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for g := range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					out, err := f.create.Execute(as(alice), in)
					if err == nil {
						ids[g], replayed[g] = out.ID, out.Replayed
					}
					errs[g] = err
				}()
			}
			close(start)
			wg.Wait()

			for g := range n {
				if errs[g] != nil {
					t.Fatalf("request %d: %v", g, errs[g])
				}
				if ids[g] != ids[0] {
					t.Fatalf("request %d got %s, request 0 got %s: one key, two orders", g, ids[g], ids[0])
				}
			}
			// Whoever lost the race (lookup miss then conflict, or a late lookup hit) is a replay.
			created := 0
			for _, r := range replayed {
				if !r {
					created++
				}
			}
			if created != 1 {
				t.Fatalf("%d of %d answers were not replays, want exactly the 1 that created", created, n)
			}
			if got := f.total(t, as(ops)); got != 1 {
				t.Fatalf("%d orders stored, want 1", got)
			}
			if got := f.events.created(); got != 1 {
				t.Fatalf("%d OrderCreated published, want 1", got)
			}
		})

		t.Run("concurrent same key different items: one wins, the rest are key reuse", func(t *testing.T) {
			f := newIdemFixture(repo)

			const n = 6
			errs := make([]error, n)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for g := range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, errs[g] = f.create.Execute(as(alice), dto.CreateOrderInput{
						Items:          items(fmt.Sprintf("SKU-%d", g)),
						IdempotencyKey: "k-race-diff",
					})
				}()
			}
			close(start)
			wg.Wait()

			won := 0
			for g, err := range errs {
				switch {
				case err == nil:
					won++
				case !errors.Is(err, order.ErrIdempotencyKeyReused):
					t.Fatalf("request %d: err = %v, want nil or ErrIdempotencyKeyReused", g, err)
				}
			}
			if won != 1 {
				t.Fatalf("%d requests won, want exactly 1", won)
			}
			if got := f.total(t, as(ops)); got != 1 {
				t.Fatalf("%d orders stored, want 1", got)
			}
			if got := f.events.created(); got != 1 {
				t.Fatalf("%d OrderCreated published, want 1", got)
			}
		})
	})
}

// The outbox row is written in the same transaction as the order, so the proof
// that a replay does not publish again has to run through the real Transactional
// decorator, Postgres publisher and the outbox table, not a recording fake.
func TestCreateOrder_IdempotencyKey_Outbox(t *testing.T) {
	url := pgtest.URL(t)
	cfg := config.Postgres{URL: url}
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
	if err := platformpg.Up(context.Background(), cfg, postgres.Migration()); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`TRUNCATE ordering.orders, ordering.events_v1_ordercreated CASCADE`).Error; err != nil {
		t.Fatal(err)
	}

	pub, err := eventbus.NewPostgresPublisher(db, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	own := ownership.New(adminRole)
	clock := fixedClock{t: time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)}
	create := usecasex.Chain[dto.CreateOrderInput, *dto.OrderOutput](
		usecase.NewCreateOrder(postgres.NewOrderRepository(db), events.NewPublisher(pub), atomicIDs{}, clock, own),
		usecasex.Transactional[dto.CreateOrderInput, *dto.OrderOutput](postgres.NewTransactor(db)),
	)

	outbox := func() int64 {
		var n int64
		if err := db.Raw(`SELECT count(*) FROM ordering.events_v1_ordercreated`).Scan(&n).Error; err != nil {
			t.Fatal(err)
		}

		return n
	}

	in := dto.CreateOrderInput{Items: items("SKU-1"), IdempotencyKey: "k-outbox"}
	first, err := create.Execute(as(alice), in)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		again, err := create.Execute(as(alice), in)
		if err != nil || again.ID != first.ID {
			t.Fatalf("replay = %+v, %v; want %s", again, err, first.ID)
		}
	}
	if _, err := create.Execute(as(alice), dto.CreateOrderInput{Items: items("SKU-2"), IdempotencyKey: "k-outbox"}); !errors.Is(err, order.ErrIdempotencyKeyReused) {
		t.Fatalf("reuse err = %v", err)
	}
	if n := outbox(); n != 1 {
		t.Fatalf("outbox holds %d OrderCreated after 1 create + 3 replays + 1 reuse, want 1", n)
	}

	// Concurrent first use: still exactly one more row.
	race := dto.CreateOrderInput{Items: items("SKU-3"), IdempotencyKey: "k-outbox-race"}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := create.Execute(as(alice), race); err != nil {
				t.Errorf("race: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if n := outbox(); n != 2 {
		t.Fatalf("outbox holds %d OrderCreated after the race, want 2", n)
	}
}
