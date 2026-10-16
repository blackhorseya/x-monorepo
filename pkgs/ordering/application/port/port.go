// Package port declares what the ordering use cases need from the outside world.
// Implementations live in ordering/infra. This is where go-micro's "plugins"
// (broker, store, ...) become plain interfaces.
package port

import (
	"context"
	"time"
)

// IDGenerator produces new aggregate IDs.
type IDGenerator interface {
	NewID(c context.Context) (string, error)
}

// Clock abstracts time for deterministic tests.
type Clock interface {
	Now() time.Time
}
