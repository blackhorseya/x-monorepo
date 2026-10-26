// Package port declares what the identity use cases need from the outside
// world. Implementations live in identity/infra.
package port

import (
	"context"
	"time"
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
