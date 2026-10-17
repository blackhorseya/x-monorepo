package ordering_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/system"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

var errPublish = errors.New("publish failed")

// publisher stands in for the outbox; fail makes Publish error after Save ran.
type publisher struct{ fail bool }

func (p publisher) Publish(context.Context, ...order.Event) error {
	if p.fail {
		return errPublish
	}

	return nil
}

// TestCreateOrder_Transactional proves the provider wires the transaction
// decorator: the order row is saved before Publish fails, and must not survive.
func TestCreateOrder_Transactional(t *testing.T) {
	c, db, tel := openDB(t)
	rec, err := usecasex.NewRecorder(tel.Meter)
	if err != nil {
		t.Fatal(err)
	}
	repo := ordering.ProvideOrderRepository(db, tel.Logger)
	create := func(fail bool) (*dto.OrderOutput, error) {
		uc := ordering.ProvideCreateOrder(repo, ordering.ProvideTransactor(db), publisher{fail: fail}, system.NewUUIDGenerator(), system.NewClock(), rec)
		return uc.Execute(c, dto.CreateOrderInput{
			CustomerID: "c-tx",
			Items:      []dto.LineItem{{SKU: "A", Quantity: 1, UnitPrice: dto.Money{AmountMinor: 100, Currency: "USD"}}},
		})
	}

	// Positive control: without the failure the same call persists the order,
	// so "not found" below can only mean the rollback happened.
	ok, err := create(false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := repo.FindByID(c, order.ID(ok.ID)); err != nil {
		t.Fatalf("committed order: %v", err)
	}

	out, err := create(true)
	if !errors.Is(err, errPublish) || out != nil {
		t.Fatalf("create with failing publisher = %v, %v; want nil, errPublish", out, err)
	}
	var n int64
	if err := db.WithContext(c).Raw(`SELECT count(*) FROM ordering.orders WHERE customer_id = 'c-tx'`).Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("orders for c-tx = %d, want 1 (the failed create must roll back)", n)
	}
}

func openDB(t *testing.T) (context.Context, *gorm.DB, *otel.Providers) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("TEST_DATABASE_URL must be set in CI: a skipped integration test would be a false green")
		}
		t.Skip("TEST_DATABASE_URL not set")
	}

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
	if err := db.WithContext(c).Exec(`DELETE FROM ordering.orders WHERE customer_id = 'c-tx'`).Error; err != nil {
		t.Fatal(err)
	}

	return c, db, tel
}
