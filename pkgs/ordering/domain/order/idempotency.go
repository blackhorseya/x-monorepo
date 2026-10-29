package order

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// Fingerprint identifies what a CreateOrder request asked for: the items, in the
// order given, as sku / quantity / unit price / currency. It is deliberately not
// normalised: the item sequence is the order's positions, so the same lines in a
// different order are a different request (and a reused key then fails loudly
// instead of silently returning an order that differs from what was asked).
// Every field is length-prefixed, so no two item lists share a pre-image.
func Fingerprint(items []LineItem) string {
	h := sha256.New()
	field := func(s string) {
		h.Write([]byte(strconv.Itoa(len(s))))
		h.Write([]byte{':'})
		h.Write([]byte(s))
	}
	for _, it := range items {
		field(it.SKU())
		field(strconv.FormatInt(int64(it.Quantity()), 10))
		field(strconv.FormatInt(it.UnitPrice().Amount(), 10))
		field(it.UnitPrice().Currency())
		h.Write([]byte{';'})
	}

	return hex.EncodeToString(h.Sum(nil))
}
