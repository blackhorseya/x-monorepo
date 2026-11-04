package events

import (
	"context"
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

var _ port.EventPublisher = (*Recorder)(nil)

// Recorder is the structural write path of the order history (#427): it wraps the EventPublisher, so
// every order event that is published is also recorded, in the same call — and, because the use cases
// publish inside their transaction, in the same transaction. A use case that is added later cannot
// forget to record: it already publishes.
//
// What it records comes from the event (from, to, cause, time, order id). The one thing it adds is the
// actor, from the principal in the context.
//
// Each event is published first and recorded second, one after the other. Both belong to one
// transaction, so on Postgres either failure rolls back the other; the order only matters where there
// is no transaction (memory), where it avoids a history row for an event that never left.
type Recorder struct {
	next      port.EventPublisher
	store     port.HistoryStore
	adminRole string
}

// NewRecorder wraps next. adminRole is auth.admin_role: a principal holding it is an ADMIN actor.
func NewRecorder(next port.EventPublisher, store port.HistoryStore, adminRole string) *Recorder {
	return &Recorder{next: next, store: store, adminRole: adminRole}
}

func (r *Recorder) Publish(c context.Context, evs ...order.Event) error {
	for _, e := range evs {
		if err := r.next.Publish(c, e); err != nil {
			return err
		}
		kind, subject := r.actor(c, e.Cause())
		entry, err := order.NewHistoryEntry(e, kind, subject)
		if err != nil {
			return fmt.Errorf("history entry for order %s: %w", e.OrderID(), err)
		}
		if err := r.store.Append(c, entry); err != nil {
			return err
		}
	}

	return nil
}

// actor: a transition an rpc caller asked for is attributed to that caller (ADMIN, CUSTOMER, or
// ANONYMOUS when auth is off); every other cause is an event consumer and is SYSTEM, whatever the
// context carries — a consumer has no principal, and one that somehow did would still not be a person.
func (r *Recorder) actor(c context.Context, cause order.Cause) (order.ActorKind, string) {
	if cause != order.CauseRPCRequest {
		return order.ActorSystem, ""
	}

	p := contextx.PrincipalFrom(c)
	switch {
	case p.IsAnonymous():
		return order.ActorAnonymous, ""
	case p.HasRole(r.adminRole):
		return order.ActorAdmin, p.Subject
	default:
		return order.ActorCustomer, p.Subject
	}
}
