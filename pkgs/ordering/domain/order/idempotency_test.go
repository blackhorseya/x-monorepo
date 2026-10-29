package order_test

import (
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
)

func TestFingerprint(t *testing.T) {
	usd := mustMoney(t, 500, "USD")
	a := mustItem(t, "A", 1, usd)
	b := mustItem(t, "B", 2, usd)

	base := order.Fingerprint([]order.LineItem{a, b})
	if base != order.Fingerprint([]order.LineItem{a, b}) {
		t.Fatal("fingerprint is not deterministic")
	}

	differs := map[string][]order.LineItem{
		"reordered":       {b, a},
		"fewer":           {a},
		"quantity":        {a, mustItem(t, "B", 3, usd)},
		"price":           {a, mustItem(t, "B", 2, mustMoney(t, 501, "USD"))},
		"currency":        {a, mustItem(t, "B", 2, mustMoney(t, 500, "EUR"))},
		"merged same sku": {mustItem(t, "A", 1, usd), mustItem(t, "A", 1, usd), b},
	}
	for name, items := range differs {
		if order.Fingerprint(items) == base {
			t.Errorf("%s: fingerprint collides with the base request", name)
		}
	}

	// Length prefixes: moving a character across a field boundary is a different request.
	x := order.Fingerprint([]order.LineItem{mustItem(t, "AB", 1, usd)})
	y := order.Fingerprint([]order.LineItem{mustItem(t, "A", 1, usd), mustItem(t, "B", 1, usd)})
	if x == y {
		t.Fatal("different item lists share a fingerprint")
	}
}

func TestOrder_IdempotencyKey(t *testing.T) {
	usd := mustMoney(t, 500, "USD")
	items := []order.LineItem{mustItem(t, "A", 1, usd), mustItem(t, "B", 2, usd)}
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)

	keyed, err := order.New(order.Params{ID: "o-1", CustomerID: "c-1", Items: items, Now: now, IdempotencyKey: "k-1"})
	if err != nil {
		t.Fatal(err)
	}
	if keyed.IdempotencyKey() != "k-1" || keyed.RequestHash() != order.Fingerprint(items) {
		t.Fatalf("key=%q hash=%q", keyed.IdempotencyKey(), keyed.RequestHash())
	}
	if !keyed.IsReplayOf(items) {
		t.Fatal("same items must replay")
	}
	if keyed.IsReplayOf([]order.LineItem{items[1], items[0]}) {
		t.Fatal("reordered items must not replay")
	}

	plain, err := order.New(order.Params{ID: "o-2", CustomerID: "c-1", Items: items, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if plain.IdempotencyKey() != "" || plain.RequestHash() != "" || plain.IsReplayOf(items) {
		t.Fatalf("keyless order: key=%q hash=%q replay=%v", plain.IdempotencyKey(), plain.RequestHash(), plain.IsReplayOf(items))
	}

	back, err := order.Reconstitute(order.ReconstituteParams{
		ID: "o-1", CustomerID: "c-1", Items: items, Status: order.StatusPending, CreatedAt: now, UpdatedAt: now, Version: 1,
		IdempotencyKey: keyed.IdempotencyKey(), RequestHash: keyed.RequestHash(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if back.IdempotencyKey() != "k-1" || !back.IsReplayOf(items) {
		t.Fatal("key and hash must survive Reconstitute")
	}
}
