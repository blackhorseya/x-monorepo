// Package port declares what the ordering use cases need from the outside world.
// Implementations live in ordering/infra. This is where go-micro's "plugins"
// (broker, store, ...) become plain interfaces.
package port

import (
	"context"
	"errors"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
)

// IDGenerator produces new aggregate IDs.
type IDGenerator interface {
	NewID(c context.Context) (string, error)
}

// Clock abstracts time for deterministic tests.
type Clock interface {
	Now() time.Time
}

// Transactor runs fn atomically: every repository call and every event
// published inside fn commits together or not at all (the transactional
// outbox). Use cases do not call it: provider.go wraps every writing use case
// with usecasex.Transactional(tx). The memory implementation just calls fn.
type Transactor interface {
	Do(c context.Context, fn func(c context.Context) error) error
}

// Inbox records consumed event message ids (usecasex.Inbox, this BC's own
// type so wire tells BCs apart). Use cases do not call it: provider.go wraps
// every writing use case with usecasex.Idempotent(inbox), inside the
// transaction, and a redelivered message is dropped before the use case runs.
type Inbox interface {
	Record(c context.Context, id string) (first bool, err error)
}

// EventPublisher hands domain events to the outside world. Called inside
// Transactor.Do so the events share the state change's transaction.
type EventPublisher interface {
	Publish(c context.Context, events ...order.Event) error
}

// ErrCatalogUnavailable marks a failure to get an answer from the catalog (as opposed to the
// catalog answering "no such sku"). PriceQuoter implementations wrap the cause with it; the rpc
// edge maps it to UNAVAILABLE. It is deliberately not a kernel.Error: nothing about the order is wrong.
var ErrCatalogUnavailable = errors.New("catalog unavailable")
