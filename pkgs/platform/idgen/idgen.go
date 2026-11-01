// Package idgen mints the identifiers the system hands out: entity ids, event
// message ids and request ids.
//
// Ids are UUID v7: the leading 48 bits are a millisecond timestamp, so new
// primary keys land at the tail of the index instead of anywhere in it. That
// is the only reason for v7. Callers must keep treating an id as an opaque
// string:
//
//   - Nothing may order by id alone. Rows created before v7 carry v4 ids, and
//     two pods mint v7 ids with only millisecond order between them; listings
//     keep their (created_at, id) keyset and use the id as a tie-breaker.
//   - An id is an identifier, never a credential. v7 carries fewer random bits
//     and reveals when it was created. Secrets (API keys) come from
//     identity's SecretGenerator, which stays 256 bits of pure randomness.
package idgen

import (
	"fmt"

	"github.com/google/uuid"
)

// New returns a fresh UUID v7 in its canonical 36-character form. It fails
// only when the system's randomness source does.
func New() (string, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("generate uuid v7: %w", err)
	}

	return u.String(), nil
}
