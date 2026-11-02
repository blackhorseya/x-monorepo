package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/stock"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// FindBySKUs answers with the stocks that exist, once each, whatever the
// request repeats or names that was never stocked.
func TestStockRepository_FindBySKUs_Postgres(t *testing.T) {
	c := context.Background()
	cfg := config.Postgres{URL: pgtest.URL(t)}
	tel := pgtest.Telemetry(t)
	db := pgtest.DB(t, cfg, tel) // closed by a t.Cleanup registered here, before any purge below: LIFO runs the purge while the pool is open
	if err := platformpg.Up(c, cfg, postgres.Migration()); err != nil {
		t.Fatal(err)
	}
	// Own fixture: purge before (a repeated run starts clean) and after (nothing is
	// left for other tests), with the error checked.
	purge := func() {
		if err := db.WithContext(c).Exec("DELETE FROM inventory.stocks WHERE sku LIKE 'BATCH-%'").Error; err != nil {
			t.Errorf("purge BATCH stocks: %v", err)
		}
	}
	purge()
	t.Cleanup(purge)

	stocks := postgres.NewStockRepository(db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for n, sku := range []string{"BATCH-A", "BATCH-B", "BATCH-C"} {
		s, _ := stock.New(stock.SKU(sku), int32(n+1), now)
		if err := stocks.Save(c, s); err != nil {
			t.Fatal(err)
		}
	}

	got, err := stocks.FindBySKUs(c, []stock.SKU{"BATCH-C", "BATCH-A", "BATCH-A", "BATCH-NEVER"})
	if err != nil {
		t.Fatal(err)
	}
	bySKU := map[stock.SKU]*stock.Stock{}
	for _, s := range got {
		bySKU[s.SKU()] = s
	}
	if len(got) != 2 || bySKU["BATCH-A"] == nil || bySKU["BATCH-C"] == nil {
		t.Fatalf("FindBySKUs = %v, want BATCH-A and BATCH-C once each", got)
	}
	if a := bySKU["BATCH-A"]; a.OnHand() != 1 || a.Version() != 1 || !a.UpdatedAt().Equal(now) {
		t.Fatalf("BATCH-A = %+v, want on_hand 1 version 1 updated_at %v", a, now)
	}
	if cc := bySKU["BATCH-C"]; cc.OnHand() != 3 {
		t.Fatalf("BATCH-C on_hand = %d, want 3", cc.OnHand())
	}

	if none, err := stocks.FindBySKUs(c, nil); err != nil || len(none) != 0 {
		t.Fatalf("no SKUs = %v, %v, want empty", none, err)
	}
	if none, err := stocks.FindBySKUs(c, []stock.SKU{"BATCH-NEVER"}); err != nil || len(none) != 0 {
		t.Fatalf("only unknown SKUs = %v, %v, want empty", none, err)
	}
}
