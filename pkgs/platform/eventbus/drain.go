package eventbus

import (
	"context"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
)

// DefaultDrainTimeout is how long an in-flight handler may keep running after
// the router's context is cancelled (ConsumerOptions.DrainTimeout unset). It
// is below the router's CloseTimeout (NewRouter) so the router never reports
// a close timeout before the drain gives up.
const DefaultDrainTimeout = 8 * time.Second

// ackSettle is how long after the last ack the inner subscriber keeps its
// context: the SQL subscriber commits the offset (and the Redis one XACKs)
// right after the ack, through that context. Cancelling it in that window
// would roll the ack back and the message would be seen again after restart.
const ackSettle = time.Second

// Draining wraps sub so cancelling the Subscribe context stops taking new
// messages but does not abort the one being handled: it keeps a live context
// until it is acked or nacked, or timeout passes, and only then is the
// subscriber told to stop. Without it both watermill-sql and
// watermill-redisstream cancel the message's context and discard its ack the
// moment the subscription's context is cancelled, so a worker killed with
// SIGTERM mid-handler aborts the use case's transaction and sees the message
// again after restart. Register wraps every BC subscriber with it.
func Draining(sub message.Subscriber, timeout time.Duration) message.Subscriber {
	if timeout <= 0 {
		timeout = DefaultDrainTimeout
	}

	return &draining{sub: sub, timeout: timeout}
}

type draining struct {
	sub     message.Subscriber
	timeout time.Duration
}

func (d *draining) Subscribe(c context.Context, topic string) (<-chan *message.Message, error) {
	// The inner subscription lives on c's values (logger, tracing) but not
	// its cancellation: forward decides when it ends.
	inner, stop := context.WithCancel(context.WithoutCancel(c))
	in, err := d.sub.Subscribe(inner, topic)
	if err != nil {
		stop()
		return nil, err
	}
	out := make(chan *message.Message)
	go d.forward(c, in, out, stop)

	return out, nil
}

func (d *draining) Close() error { return d.sub.Close() }

// forward hands every message from in to out and waits for its outcome
// before taking the next one, as the router consumes them one at a time per
// subscription anyway. Once c is done it stops the inner subscription: at
// once when idle, after the in-flight message's outcome otherwise.
func (d *draining) forward(c context.Context, in <-chan *message.Message, out chan<- *message.Message, stop context.CancelFunc) {
	defer close(out)
	defer stop()
	var lastDone time.Time
	for {
		select {
		case <-c.Done():
			d.settle(lastDone)
			return
		case msg, ok := <-in:
			if !ok {
				return
			}
			if c.Err() != nil {
				// Shutdown began while this one was being fetched: leave it
				// unacked (rolled back / pending) for the next run.
				return
			}
			out <- msg
			if !d.await(c, msg) {
				return
			}
			lastDone = time.Now()
		}
	}
}

// await reports whether msg reached an outcome: immediately acked or nacked,
// or, once c is done, within timeout. False means the handler outlived the
// drain and is abandoned (its context is cancelled by the caller's stop).
func (d *draining) await(c context.Context, msg *message.Message) bool {
	select {
	case <-msg.Acked():
		return true
	case <-msg.Nacked():
		return true
	case <-c.Done():
	}
	t := time.NewTimer(d.timeout)
	defer t.Stop()
	select {
	case <-msg.Acked():
		return true
	case <-msg.Nacked():
		return true
	case <-t.C:
		return false
	}
}

// settle waits until ackSettle has passed since lastDone.
func (d *draining) settle(lastDone time.Time) {
	if lastDone.IsZero() {
		return
	}
	if wait := ackSettle - time.Since(lastDone); wait > 0 {
		time.Sleep(wait)
	}
}
