package order

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// fingerprintV2Prefix tells a stored request hash which fingerprint it is. v1 hashes are
// bare hex, so the prefix cannot collide with one.
const fingerprintV2Prefix = "v2:"

// FingerprintV2 identifies what a CreateOrder request asked for: the lines, in the order
// given, as sku / quantity. The price is not part of it: the caller cannot name one, the
// catalog does. It is deliberately not normalised: the line sequence is the order's
// positions, so the same lines in a different order are a different request (and a reused
// key then fails loudly instead of silently returning an order that differs from what was
// asked). Every field is length-prefixed, so no two line lists share a pre-image.
func FingerprintV2(lines []RequestLine) string {
	h := sha256.New()
	field := func(s string) {
		h.Write([]byte(strconv.Itoa(len(s))))
		h.Write([]byte{':'})
		h.Write([]byte(s))
	}
	for _, l := range lines {
		field(l.sku)
		field(strconv.FormatInt(int64(l.quantity), 10))
		h.Write([]byte{';'})
	}

	return fingerprintV2Prefix + hex.EncodeToString(h.Sum(nil))
}

// Fingerprint is the v1 fingerprint: the items as sku / quantity / unit price / currency,
// from when the caller typed the price. Orders stored before server pricing carry it; new
// orders carry FingerprintV2. It stays so those keys keep answering the same retry the same
// way (see Order.IsReplayOf), same encoding as ever: the stored hashes must still match.
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
