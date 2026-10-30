package order_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
)

// TestFingerprint is the v1 fingerprint, kept for the orders stored before the price came
// from the catalog: it covers sku, quantity, unit price and currency.
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

func lines(t *testing.T, pairs ...any) []order.RequestLine {
	t.Helper()
	var out []order.RequestLine
	for i := 0; i < len(pairs); i += 2 {
		l, err := order.NewRequestLine(pairs[i].(string), int32(pairs[i+1].(int)))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, l)
	}

	return out
}

func TestNewRequestLine(t *testing.T) {
	l, err := order.NewRequestLine("A", 2)
	if err != nil || l.SKU() != "A" || l.Quantity() != 2 {
		t.Fatalf("line = %+v, %v", l, err)
	}
	if _, err := order.NewRequestLine("", 1); !errors.Is(err, order.ErrInvalidSKU) {
		t.Fatalf("empty sku: %v", err)
	}
	if _, err := order.NewRequestLine("A", 0); !errors.Is(err, order.ErrInvalidQuantity) {
		t.Fatalf("zero quantity: %v", err)
	}
}

// v2 covers what the caller can say: sku and quantity, in order. The price is the
// catalog's, so it can never be part of the question.
func TestFingerprintV2(t *testing.T) {
	base := order.FingerprintV2(lines(t, "A", 1, "B", 2))
	if !strings.HasPrefix(base, "v2:") {
		t.Fatalf("fingerprint %q carries no version prefix", base)
	}
	if base != order.FingerprintV2(lines(t, "A", 1, "B", 2)) {
		t.Fatal("fingerprint is not deterministic")
	}
	for name, other := range map[string][]order.RequestLine{
		"reordered":       lines(t, "B", 2, "A", 1),
		"fewer":           lines(t, "A", 1),
		"quantity":        lines(t, "A", 1, "B", 3),
		"merged same sku": lines(t, "A", 1, "A", 1, "B", 2),
	} {
		if order.FingerprintV2(other) == base {
			t.Errorf("%s: fingerprint collides with the base request", name)
		}
	}
	// Length prefixes: moving a character across a field boundary is a different request.
	if order.FingerprintV2(lines(t, "AB", 1)) == order.FingerprintV2(lines(t, "A", 1, "B", 1)) {
		t.Fatal("different line lists share a fingerprint")
	}
	// v1 and v2 never collide, even on the same lines, so a stored hash says which one it is.
	usd := mustMoney(t, 500, "USD")
	if order.Fingerprint([]order.LineItem{mustItem(t, "A", 1, usd)}) == order.FingerprintV2(lines(t, "A", 1)) {
		t.Fatal("v1 and v2 fingerprints are indistinguishable")
	}
}

func TestOrder_IdempotencyKey(t *testing.T) {
	usd := mustMoney(t, 500, "USD")
	items := []order.LineItem{mustItem(t, "A", 1, usd), mustItem(t, "B", 2, usd)}
	asked := lines(t, "A", 1, "B", 2)
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)

	keyed, err := order.New(order.Params{ID: "o-1", CustomerID: "c-1", Items: items, Now: now, IdempotencyKey: "k-1"})
	if err != nil {
		t.Fatal(err)
	}
	if keyed.IdempotencyKey() != "k-1" || keyed.RequestHash() != order.FingerprintV2(asked) {
		t.Fatalf("key=%q hash=%q, want the v2 fingerprint of the request", keyed.IdempotencyKey(), keyed.RequestHash())
	}
	if !keyed.IsReplayOf(asked) {
		t.Fatal("same lines must replay")
	}
	if keyed.IsReplayOf(lines(t, "B", 2, "A", 1)) || keyed.IsReplayOf(lines(t, "A", 1, "B", 3)) {
		t.Fatal("reordered or changed lines must not replay")
	}

	plain, err := order.New(order.Params{ID: "o-2", CustomerID: "c-1", Items: items, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if plain.IdempotencyKey() != "" || plain.RequestHash() != "" || plain.IsReplayOf(asked) {
		t.Fatalf("keyless order: key=%q hash=%q replay=%v", plain.IdempotencyKey(), plain.RequestHash(), plain.IsReplayOf(asked))
	}

	back, err := order.Reconstitute(order.ReconstituteParams{
		ID: "o-1", CustomerID: "c-1", Items: items, Status: order.StatusPending, CreatedAt: now, UpdatedAt: now, Version: 1,
		IdempotencyKey: keyed.IdempotencyKey(), RequestHash: keyed.RequestHash(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if back.IdempotencyKey() != "k-1" || !back.IsReplayOf(asked) {
		t.Fatal("key and hash must survive Reconstitute")
	}
}

// An order stored before server pricing carries the v1 fingerprint (sku, quantity, the
// price the caller typed, currency). Its key must keep answering the same retry the same
// way: the lines asked again are compared as v1 against the price snapshot on the order.
func TestOrder_LegacyV1Replay(t *testing.T) {
	usd := mustMoney(t, 500, "USD")
	items := []order.LineItem{mustItem(t, "A", 1, usd), mustItem(t, "B", 2, usd)}
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)

	legacy, err := order.Reconstitute(order.ReconstituteParams{
		ID: "o-1", CustomerID: "c-1", Items: items, Status: order.StatusConfirmed, CreatedAt: now, UpdatedAt: now, Version: 2,
		IdempotencyKey: "k-old", RequestHash: order.Fingerprint(items),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !legacy.IsReplayOf(lines(t, "A", 1, "B", 2)) {
		t.Fatal("the retry of a v1 order must still replay it")
	}
	for name, other := range map[string][]order.RequestLine{
		"reordered": lines(t, "B", 2, "A", 1),
		"quantity":  lines(t, "A", 1, "B", 3),
		"sku":       lines(t, "A", 1, "C", 2),
		"fewer":     lines(t, "A", 1),
		"more":      lines(t, "A", 1, "B", 2, "C", 1),
	} {
		if legacy.IsReplayOf(other) {
			t.Errorf("%s: a different request replayed a v1 order", name)
		}
	}
	// A v1 hash that does not match its own snapshot (tampered row) replays nothing.
	broken, err := order.Reconstitute(order.ReconstituteParams{
		ID: "o-2", CustomerID: "c-1", Items: items, Status: order.StatusPending, CreatedAt: now, UpdatedAt: now, Version: 1,
		IdempotencyKey: "k-bad", RequestHash: order.Fingerprint(items[:1]),
	})
	if err != nil {
		t.Fatal(err)
	}
	if broken.IsReplayOf(lines(t, "A", 1, "B", 2)) {
		t.Fatal("a v1 hash that does not match the snapshot replayed")
	}
}
