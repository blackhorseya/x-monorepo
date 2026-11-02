package postgres_test

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
)

// txProbe is the provider fake of docs/payment.md §9「StartPayment 的 provider 呼叫不在 DB transaction
// 內」: at the moment CreateCheckout is called it reads how many connections of the repository's own pool
// are checked out. A transaction holds one for its whole life, and nothing else here uses the pool, so
// InUse > 0 at that moment means the provider is being called with a database connection held.
// The pool it reads is the one behind the same *gorm.DB the repository and the transactor use.
type txProbe struct {
	*mock.Provider
	pool *sql.DB

	mu    sync.Mutex
	inUse []int
}

func (p *txProbe) CreateCheckout(c context.Context, req dto.ProviderCheckoutInput) (dto.ProviderCheckoutOutput, error) {
	p.mu.Lock()
	p.inUse = append(p.inUse, p.pool.Stats().InUse)
	p.mu.Unlock()

	return p.Provider.CreateCheckout(c, req)
}

// observed returns the InUse readings, one per CreateCheckout call.
func (p *txProbe) observed() []int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]int(nil), p.inUse...)
}

// outsideTx is the assertion: the provider was called at least once (a probe that was never reached
// proves nothing) and never with a connection held.
func outsideTx(readings []int) bool {
	if len(readings) == 0 {
		return false
	}
	for _, n := range readings {
		if n != 0 {
			return false
		}
	}

	return true
}

// fixedClock is the clock of the payment fixtures (t0), so the window is open whatever day this runs.
type fixedClock struct{}

func (fixedClock) Now() time.Time { return t0 }

func startEnv(t *testing.T) (*usecase.StartPayment, *txProbe, *postgres.Transactor) {
	t.Helper()
	db := testDB(t)
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	repo := postgres.NewPaymentRepository(db)
	if err = repo.Save(context.Background(), newPayment(t, "pay-1", "ord-1")); err != nil {
		t.Fatal(err)
	}
	probe := &txProbe{Provider: mock.New(mock.Options{CheckoutTTL: 10 * time.Minute, Now: fixedClock{}.Now}), pool: pool}
	tx := postgres.NewTransactor(db)

	return usecase.NewStartPayment(repo, probe, tx, fixedClock{}), probe, tx
}

var startIn = dto.StartPaymentInput{OrderID: "ord-1", CustomerID: "cust"}

// The real use case on a real pool calls the provider with no connection held, even though the write
// that follows is a transaction; and the write did commit.
func TestStartPayment_ProviderIsCalledOutsideATransaction(t *testing.T) {
	start, probe, _ := startEnv(t)

	out, err := start.Execute(context.Background(), startIn)
	if err != nil || out.CheckoutURL == "" {
		t.Fatalf("start: %+v, %v", out, err)
	}
	if readings := probe.observed(); !outsideTx(readings) {
		t.Fatalf("CreateCheckout saw InUse readings %v; want at least one reading, all 0", readings)
	}
}

// Fault injection: the assertion above must be able to go red. The same use case run INSIDE a
// transaction (what decorateTx's Transactional would do to it) is seen by the probe holding a
// connection, so outsideTx is false. If the probe measured some other pool, or nothing, this test
// would fail because the reading would stay 0.
func TestStartPayment_TheProbeSeesAProviderCallInsideATransaction(t *testing.T) {
	start, probe, tx := startEnv(t)

	err := tx.Do(context.Background(), func(c context.Context) error {
		_, err := start.Execute(c, startIn)

		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	readings := probe.observed()
	if len(readings) == 0 || readings[0] < 1 {
		t.Fatalf("readings %v: the probe did not see the connection the surrounding transaction holds", readings)
	}
	if outsideTx(readings) {
		t.Fatalf("readings %v passed the outside-a-transaction assertion for a call made inside one", readings)
	}
}
