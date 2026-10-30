package ordering_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"gorm.io/gorm"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/metrics"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/ownership"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/system"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
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

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

// TestCreateOrder_Transactional proves the provider wires the transaction
// decorator: the order row is saved before Publish fails, and must not survive.
func TestCreateOrder_Transactional(t *testing.T) {
	c, db, tel := openDB(t)
	rec, err := usecasex.NewRecorder(tel.Meter)
	if err != nil {
		t.Fatal(err)
	}
	repo := ordering.ProvideOrderRepository(db, tel.Logger)
	reader := sdkmetric.NewManualReader()
	results, err := metrics.NewCreateOrderResults(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	if err != nil {
		t.Fatal(err)
	}
	create := func(fail bool) (*dto.OrderOutput, error) {
		uc := ordering.ProvideCreateOrder(repo, ordering.ProvideTransactor(db), ordering.ProvideInbox(db), publisher{fail: fail}, system.NewUUIDGenerator(), system.NewClock(), ownership.New("admin"), memory.NewFlatPriceList(100, "USD"), rec, results)
		return uc.Execute(c, dto.CreateOrderInput{
			CustomerID: "c-tx",
			Items:      []dto.OrderLine{{SKU: "A", Quantity: 1}},
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

	// The counter sits outside the transaction: one committed create is "created",
	// the rolled-back one is not counted at all.
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(c, &rm); err != nil {
		t.Fatal(err)
	}
	var created, total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "ordering.create_order" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				total += dp.Value
				if v, _ := dp.Attributes.Value("result"); v.AsString() == metrics.ResultCreated {
					created += dp.Value
				}
			}
		}
	}
	if created != 1 || total != 1 {
		t.Fatalf("ordering.create_order created=%d total=%d, want 1 and 1 (a rolled-back create must not count)", created, total)
	}
}

func openDB(t *testing.T) (context.Context, *gorm.DB, *otel.Providers) {
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
	if err := db.WithContext(c).Exec(`DELETE FROM ordering.orders WHERE customer_id = 'c-tx'`).Error; err != nil {
		t.Fatal(err)
	}

	return c, db, tel
}
