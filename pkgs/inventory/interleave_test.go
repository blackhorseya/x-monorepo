package inventory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/stock"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
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

// The stock version cannot see a reservation that changed after it was read:
// CommitStock reads o1 as RESERVED, then another transaction expires o1 and
// o2 reserves the very units it gave back, and only then does CommitStock
// load the (now fresh) stock. Without a lock on the reservation itself it
// would sell o1 out of o2's hold. The commit must lose (retryable), and the
// retry must see EXPIRED, find nothing available and end COMMIT_SHORT, with
// o2's hold intact.
func TestCommitStock_ReservationChangedUnderneath(t *testing.T) {
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
	stocks := inventory.ProvideStockRepository(db, tel.Logger)
	reservations := inventory.ProvideReservationRepository(db)
	tx := inventory.ProvideTransactor(db)
	inbox := inventory.ProvideInbox(db)
	now := clockAt{at: time.Now().UTC()}
	old := clockAt{at: now.at.Add(-25 * time.Hour)}

	reserve := func(orderID string, clock clockAt) {
		t.Helper()
		uc := inventory.ProvideReserveStock(reservations, stocks, tx, inbox, publisher{}, clock, rec)
		out, err := uc.Execute(c, dto.ReserveStockInput{OrderID: orderID, Lines: []dto.Line{{SKU: "SKU-RACE", Quantity: 2}}})
		if err != nil || out.Status != "RESERVED" {
			t.Fatalf("reserve %s = %+v, %v", orderID, out, err)
		}
	}
	if _, err := inventory.ProvideSetStock(stocks, tx, inbox, now, rec).Execute(c, dto.SetStockInput{SKU: "SKU-RACE", OnHand: 2}); err != nil {
		t.Fatal(err)
	}
	reserve("o-race-1", old)

	interleaved := &beforeFind{Repository: stocks}
	interleaved.hook = func() {
		out, err := inventory.ProvideExpireReservations(reservations, stocks, tx, inbox, publisher{}, now, rec).Execute(c, dto.ExpireReservationsInput{OlderThan: 24 * time.Hour})
		if err != nil || len(out.Expired) != 1 {
			t.Fatalf("expire = %+v, %v", out, err)
		}
		reserve("o-race-2", now)
	}

	commit1 := inventory.ProvideCommitStock(reservations, interleaved, tx, inbox, publisher{}, now, rec)
	out, err := commit1.Execute(c, dto.CommitStockInput{OrderID: "o-race-1"})
	if !errors.Is(err, kernel.ErrConcurrentUpdate) {
		t.Fatalf("commit over a changed reservation = %+v, %v; want a retryable concurrent update", out, err)
	}

	// The failed attempt rolled back; the retry reloads and sees EXPIRED.
	retry := inventory.ProvideCommitStock(reservations, stocks, tx, inbox, publisher{}, now, rec)
	out, err = retry.Execute(c, dto.CommitStockInput{OrderID: "o-race-1"})
	if err != nil || out.Status != "COMMIT_SHORT" {
		t.Fatalf("retry = %+v, %v; want COMMIT_SHORT", out, err)
	}
	s, err := stocks.FindBySKU(c, "SKU-RACE")
	if err != nil || s.OnHand() != 2 || s.Reserved() != 2 {
		t.Fatalf("stock = %+v, %v; want on_hand 2 reserved 2 (o2's hold intact)", s, err)
	}
	out, err = retry.Execute(c, dto.CommitStockInput{OrderID: "o-race-2"})
	if err != nil || out.Status != "COMMITTED" {
		t.Fatalf("o2 commit = %+v, %v; want COMMITTED", out, err)
	}
}
