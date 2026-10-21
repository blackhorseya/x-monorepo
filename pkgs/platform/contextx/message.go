package contextx

import "context"

type messageIDKey struct{}

// WithMessageID returns a child context carrying the id of the event message
// being consumed. eventbus.Register sets it before a handler runs; the rpc
// path never does.
func WithMessageID(c context.Context, id string) context.Context {
	return context.WithValue(c, messageIDKey{}, id)
}

// MessageIDFrom returns the id of the message being consumed, or "" outside a
// consumer handler.
func MessageIDFrom(c context.Context) string {
	id, _ := c.Value(messageIDKey{}).(string)
	return id
}
