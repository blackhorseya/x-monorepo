package inventory_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/system"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

var errPublish = errors.New("publish failed")

type publisher struct{ fail bool }

func (p publisher) Publish(context.Context, ...reservation.Event) error {
	if p.fail {
		return errPublish
	}

	return nil
}

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

// TestReserveStock_Transactional proves the provider wires the transaction
// decorator around a use case with several writes: the stock row and the
// reservation are both written before Publish fails, and neither may survive.
func TestReserveStock_Transactional(t *testing.T) {
	url := pgtest.URL(t)

	c := context.Background()
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
	if err := platformpg.Up(c, cfg, postgres.Migration()); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DELETE FROM inventory.reservations WHERE order_id LIKE 'o-tx-%'`,
		`DELETE FROM inventory.stocks WHERE sku = 'SKU-TX'`,
	} {
		if err := db.WithContext(c).Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}

	rec, err := usecasex.NewRecorder(tel.Meter)
	if err != nil {
		t.Fatal(err)
	}
	stocks := inventory.ProvideStockRepository(db, tel.Logger)
	reservations := inventory.ProvideReservationRepository(db)
	tx := inventory.ProvideTransactor(db)
	inbox := inventory.ProvideInbox(db)
	clock := system.NewClock()
	reserve := func(orderID string, fail bool) (*dto.ReservationOutput, error) {
		uc := inventory.ProvideReserveStock(reservations, stocks, tx, inbox, publisher{fail: fail}, clock, rec)
		return uc.Execute(c, dto.ReserveStockInput{OrderID: orderID, Lines: []dto.Line{{SKU: "SKU-TX", Quantity: 2}}})
	}
	getStock := inventory.ProvideGetStock(stocks, rec)

	if _, err := inventory.ProvideSetStock(stocks, tx, inbox, clock, memory.NewAnyProductCatalog(), rec).Execute(c, dto.SetStockInput{SKU: "SKU-TX", OnHand: 5}); err != nil {
		t.Fatalf("set stock: %v", err)
	}

	// Positive control: the same call without the failure reserves 2.
	if _, err := reserve("o-tx-ok", false); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	s, err := getStock.Execute(c, dto.GetStockInput{SKU: "SKU-TX"})
	if err != nil || s.Reserved != 2 {
		t.Fatalf("after committed reserve: %+v, %v; want reserved 2", s, err)
	}

	out, err := reserve("o-tx-fail", true)
	if !errors.Is(err, errPublish) || out != nil {
		t.Fatalf("reserve with failing publisher = %v, %v; want nil, errPublish", out, err)
	}
	s, err = getStock.Execute(c, dto.GetStockInput{SKU: "SKU-TX"})
	if err != nil || s.Reserved != 2 {
		t.Fatalf("after failed reserve: %+v, %v; want reserved still 2", s, err)
	}
	if _, err := reservations.FindByOrderID(c, "o-tx-fail"); !errors.Is(err, reservation.ErrNotFound) {
		t.Fatalf("failed reservation: err = %v, want ErrNotFound", err)
	}
}
