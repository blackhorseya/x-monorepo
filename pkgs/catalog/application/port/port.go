// Package port declares what the catalog use cases need from the outside
// world. Implementations live in catalog/infra.
package port

import (
	"context"
	"time"
)

// Clock abstracts time for deterministic tests.
type Clock interface {
	Now() time.Time
}

// Transactor runs fn atomically: every repository call inside fn commits
// together or not at all. Use cases do not call it: provider.go wraps every
// writing use case with usecasex.Transactional(tx). The memory implementation
// just calls fn.
type Transactor interface {
	Do(c context.Context, fn func(c context.Context) error) error
}
