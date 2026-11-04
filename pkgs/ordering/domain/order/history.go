package order

import "time"

// ActorKind is who made an order move, as a fixed enumeration.
type ActorKind uint8

const (
	ActorUnspecified ActorKind = iota
	// ActorCustomer: an authenticated rpc caller without the admin role.
	ActorCustomer
	// ActorAdmin: an authenticated rpc caller with auth.admin_role.
	ActorAdmin
	// ActorSystem: an event consumer (no principal exists on that path).
	ActorSystem
	// ActorAnonymous: an rpc call with no principal (auth.mode=off).
	ActorAnonymous
)

func (x ActorKind) String() string {
	switch x {
	case ActorCustomer:
		return "CUSTOMER"
	case ActorAdmin:
		return "ADMIN"
	case ActorSystem:
		return "SYSTEM"
	case ActorAnonymous:
		return "ANONYMOUS"
	default:
		return "UNSPECIFIED"
	}
}

// ParseActorKind is the inverse of String for the four real kinds; UNSPECIFIED is not storable.
func ParseActorKind(s string) (ActorKind, error) {
	for _, k := range []ActorKind{ActorCustomer, ActorAdmin, ActorSystem, ActorAnonymous} {
		if k.String() == s {
			return k, nil
		}
	}

	return ActorUnspecified, ErrInvalidActor
}

// HistoryEntry is one row of an order's append-only history: one transition, who made it and why.
// It is built from the event the aggregate raised, so the history can only say what the outbox says.
// Seq is 0 until the store assigns it (1..N per order, in write order).
type HistoryEntry struct {
	orderID      ID
	seq          int
	from         Status
	to           Status
	occurredAt   time.Time
	actorKind    ActorKind
	actorSubject string
	cause        Cause
}

// NewHistoryEntry records e. A customer or admin is identified by subject; a system or anonymous
// actor has none, and an unspecified actor is refused.
func NewHistoryEntry(e Event, kind ActorKind, subject string) (HistoryEntry, error) {
	if err := checkActor(kind, subject); err != nil {
		return HistoryEntry{}, err
	}

	return HistoryEntry{
		orderID: e.OrderID(), from: e.From(), to: e.To(), occurredAt: e.OccurredAt(),
		actorKind: kind, actorSubject: subject, cause: e.Cause(),
	}, nil
}

func checkActor(kind ActorKind, subject string) error {
	switch kind {
	case ActorCustomer, ActorAdmin:
		if subject == "" {
			return ErrInvalidActor
		}
	case ActorSystem, ActorAnonymous:
		if subject != "" {
			return ErrInvalidActor
		}
	default:
		return ErrInvalidActor
	}

	return nil
}

// HistoryParams carry a stored row back into a HistoryEntry.
type HistoryParams struct {
	OrderID      ID
	Seq          int
	From         Status
	To           Status
	OccurredAt   time.Time
	ActorKind    ActorKind
	ActorSubject string
	Cause        Cause
}

// ReconstituteHistoryEntry rebuilds a stored row; a row that breaks the table's own rules is an error.
func ReconstituteHistoryEntry(p HistoryParams) (HistoryEntry, error) {
	if p.OrderID == "" {
		return HistoryEntry{}, ErrInvalidID
	}
	if p.Seq < 1 || p.To == StatusUnknown || p.OccurredAt.IsZero() {
		return HistoryEntry{}, ErrInvalidHistory
	}
	if err := checkActor(p.ActorKind, p.ActorSubject); err != nil {
		return HistoryEntry{}, err
	}

	return HistoryEntry{
		orderID: p.OrderID, seq: p.Seq, from: p.From, to: p.To, occurredAt: p.OccurredAt,
		actorKind: p.ActorKind, actorSubject: p.ActorSubject, cause: p.Cause,
	}, nil
}

func (x HistoryEntry) OrderID() ID           { return x.orderID }
func (x HistoryEntry) Seq() int              { return x.seq }
func (x HistoryEntry) From() Status          { return x.from }
func (x HistoryEntry) To() Status            { return x.to }
func (x HistoryEntry) OccurredAt() time.Time { return x.occurredAt }
func (x HistoryEntry) ActorKind() ActorKind  { return x.actorKind }
func (x HistoryEntry) ActorSubject() string  { return x.actorSubject }
func (x HistoryEntry) Cause() Cause          { return x.cause }

// Event names the transition by the state it arrived in: CREATED (the order exists, PENDING),
// CONFIRMED, PAID, COMPLETED, CANCELLED.
func (x HistoryEntry) Event() string {
	if x.to == StatusPending {
		return "CREATED"
	}

	return x.to.String()
}
