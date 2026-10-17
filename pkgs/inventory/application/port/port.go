// Package port declares what the inventory use cases need from the outside
// world. Implementations live in inventory/infra.
package port

import (
	"context"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
)

// Clock abstracts time for deterministic tests.
type Clock interface {
	Now() time.Time
}

// Transactor runs fn atomically: every repository call and every event
// published inside fn commits together or not at all (the transactional
// outbox). The memory implementation just calls fn.
type Transactor interface {
	Do(c context.Context, fn func(c context.Context) error) error
}

// EventPublisher hands domain events to the outside world. Called inside
// Transactor.Do so the events share the state change's transaction.
type EventPublisher interface {
	Publish(c context.Context, events ...reservation.Event) error
}
