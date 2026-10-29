// Package port declares what the identity use cases need from the outside
// world. Implementations live in identity/infra.
package port

import (
	"context"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
)

// Clock abstracts time for deterministic tests.
type Clock interface {
	Now() time.Time
}

// IDGenerator mints account and key ids.
type IDGenerator interface {
	NewID(c context.Context) (string, error)
}

// SecretGenerator mints the plaintext of a new API key: a recognisable
// prefix plus enough randomness that guessing is hopeless. The domain hashes
// it; only the caller of CreateApiKey ever sees it.
type SecretGenerator interface {
	NewSecret(c context.Context) (string, error)
}

// Transactor runs fn atomically. Use cases do not call it: provider.go wraps
// every writing use case with usecasex.Transactional(tx). The memory
// implementation just calls fn.
type Transactor interface {
	Do(c context.Context, fn func(c context.Context) error) error
}

// AdminCensus lists the effective admins (account.AdminStanding: ACTIVE, holds
// role, at least one key usable at now) and, inside the transaction the caller
// is in, locks them so that two writers deciding whether somebody is the last
// admin cannot both see the other as still there. Call it before loading the
// aggregates the writer changes: whatever is read after the lock is current.
// Outside a transaction the lock is released at once, i.e. there is none.
//
// The returned time is the instant "usable key" was judged, read once the lock
// is held (a writer queued behind another can wait a while, and a key may
// expire meanwhile). The caller uses it as its own "now" for the rest of the
// operation, so the census and the decision never disagree about expiry.
type AdminCensus interface {
	LockAdmins(c context.Context, role string) ([]account.AdminStanding, time.Time, error)
}
