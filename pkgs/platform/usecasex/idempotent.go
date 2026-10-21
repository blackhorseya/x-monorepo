package usecasex

import (
	"context"
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// Inbox records which event messages a bounded context has consumed, so a
// redelivery (the broker is at-least-once, a poison requeue, a crash after
// the ack was lost) does not run the use case twice. The record must share
// the use case's transaction: a failed attempt rolls it back and the retry
// records it again (platform/postgres.NewInbox does, through the context).
type Inbox interface {
	// Record notes id as consumed. first is false when it already was.
	Record(c context.Context, id string) (first bool, err error)
}

// ErrDuplicateMessage is what Idempotent returns for a message the inbox has
// already seen. It is a domain-shaped error (kernel.Error, conflict) so the
// consumer adapters drop it — acknowledged, never retried, never poison — and
// Logging reports it at Info as a rejection, not a failure.
var ErrDuplicateMessage = kernel.NewError(kernel.KindConflict, "message.duplicate", "message already consumed")

// Idempotent runs next once per event message: the message id comes from the
// context (contextx.WithMessageID, set by eventbus.Register), is recorded in
// inbox before next runs, and a second delivery returns ErrDuplicateMessage
// without touching any aggregate. Outside a consumer (no message id — the
// rpc path) or with a nil inbox (no database: in-memory is at most once
// anyway) it is a pass-through. Place it inside Transactional so the record
// rolls back with the use case.
func Idempotent[I, O any](inbox Inbox) Decorator[I, O] {
	return func(next UseCase[I, O]) UseCase[I, O] {
		if inbox == nil {
			return next
		}

		return Func[I, O](func(c context.Context, in I) (O, error) {
			id := contextx.MessageIDFrom(c)
			if id == "" {
				return next.Execute(c, in)
			}
			first, err := inbox.Record(c, id)
			if err != nil {
				var zero O
				return zero, fmt.Errorf("record message %s: %w", id, err)
			}
			if !first {
				var zero O
				return zero, ErrDuplicateMessage
			}

			return next.Execute(c, in)
		})
	}
}
