// Package eventbus is the Watermill wiring every bounded context shares:
// the transactional outbox (watermill-sql on Postgres, publishing inside the
// use case's transaction), the subscriber/router that drives consumer
// adapters, and the error contract between them.
//
// Why Watermill: message.Publisher / message.Subscriber are the port; the
// Postgres pub/sub is the implementation apps use today (no broker to run,
// exactly-once-committed thanks to the shared transaction), and swapping in
// NATS / Kafka / SQS later is a different pub/sub behind the same Router and
// the same handlers (Watermill's forwarder component turns the SQL table into
// a classic outbox in front of a broker).
//
// Topics are proto full names ("ordering.v1.OrderCreated"). The first segment
// names the owning bounded context; its Postgres schema holds the topic's
// tables (see TableName), and the BC ships their DDL in its migrations —
// services never create tables at runtime.
package eventbus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/message/router/middleware"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
)

// ErrDrop is returned by a handler to say the message is poison or no longer
// applicable (undecodable, or the domain rejected it): acknowledge it, do not
// retry, do not send it to the poison queue. Any other error is retried and,
// after Retry gives up, parked on the poison topic.
var ErrDrop = errors.New("eventbus: drop message")

// Handler consumes one message payload (a serialised proto). See ErrDrop.
type Handler interface {
	Handle(c context.Context, payload []byte) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(c context.Context, payload []byte) error

func (f HandlerFunc) Handle(c context.Context, payload []byte) error { return f(c, payload) }

// Subscription binds a Handler to a topic. BCs expose theirs from provider.go
// (Subscriptions); apps register them on a Router.
type Subscription struct {
	Topic   string
	Handler Handler
}

// Metadata keys set by Publish-side decorators and read by handlers.
const (
	// MetaKey is the partition / ordering key (aggregate id).
	MetaKey = "key"
	// MetaOccurredAt is the domain time of the event, RFC 3339.
	MetaOccurredAt = "occurred_at"
)

// TableName maps a topic to the Postgres table holding its messages:
// "ordering.v1.OrderCreated" → "ordering.events_v1_ordercreated". The owning
// BC's schema comes first so each BC's migrations own their tables.
func TableName(topic string) string {
	schema, rest, _ := strings.Cut(topic, ".")

	return schema + ".events_" + strings.ToLower(strings.ReplaceAll(rest, ".", "_"))
}

// OffsetsTableName is TableName's consumer-group offsets table.
func OffsetsTableName(topic string) string { return TableName(topic) + "_offsets" }

// PoisonTopic is where a BC's router parks messages its handlers kept
// failing on: "<bc>.poison" (table <bc>.events_poison).
func PoisonTopic(bc string) string { return bc + ".poison" }

// NewLogger adapts slog for Watermill. Watermill logs every message at Info
// and its internals at Debug/Trace; both are demoted one level so a healthy
// router is quiet at info.
func NewLogger(l *slog.Logger) watermill.LoggerAdapter {
	return watermill.NewSlogLoggerWithLevelMapping(l.With("component", "watermill"), map[slog.Level]slog.Level{
		slog.LevelInfo:  slog.LevelDebug,
		slog.LevelDebug: slog.LevelDebug - 4,
	})
}

// NewGoChannel is the in-memory pub/sub apps use without a database: at most
// once, same process only. Nothing is retained, so a topic without a running
// subscriber simply loses its messages — like a broker with no consumer.
func NewGoChannel(l *slog.Logger) *gochannel.GoChannel {
	return gochannel.NewGoChannel(gochannel.Config{}, NewLogger(l))
}

// NewRouter builds a Watermill router with the middleware every consumer
// shares, outermost first:
//
//  1. Recoverer: a panic becomes an error, not a dead worker.
//  2. tracing: the producer's trace context (traceparent in metadata) becomes
//     the parent of a CONSUMER span, so Tempo shows rpc → outbox → handler.
//
// The error contract (poison queue, retry, ErrDrop) is per bounded context —
// each BC parks failed messages on its own poison topic — so it is added per
// handler by Register.
func NewRouter(l *slog.Logger, tel *otel.Providers) (*message.Router, error) {
	r, err := message.NewRouter(message.RouterConfig{CloseTimeout: 10 * time.Second}, NewLogger(l))
	if err != nil {
		return nil, err
	}

	r.AddMiddleware(
		middleware.Recoverer,
		tracing{propagator: tel.Propagator, tracer: tel.Tracer.Tracer(scopeName)}.middleware,
	)

	return r, nil
}

// ConsumerOptions configure the error contract of one BC's handlers (Register).
type ConsumerOptions struct {
	// Poison receives messages a handler failed on after every retry
	// (middleware.PoisonQueue). Required.
	Poison message.Publisher
	// PoisonTopic names the poison topic (PoisonTopic(bc)). Required.
	PoisonTopic string
	// MaxRetries before a message is parked (default 3, exponential backoff
	// from 100ms to 5s).
	MaxRetries int
}

// Register adds one handler per subscription to r, all reading from sub, each
// wrapped (inside the router's middleware, outermost first) in:
//
//  1. PoisonQueue: after retries, park the message on opts.PoisonTopic and ack.
//  2. Retry with backoff.
//  3. drop: ErrDrop is logged and acknowledged (never retried, never poison).
//
// Handler names are the topics, so Watermill's own metrics/logs name them.
// One router can serve several BCs: call Register once per BC with its options.
func Register(r *message.Router, sub message.Subscriber, opts ConsumerOptions, subs ...Subscription) error {
	if opts.Poison == nil || opts.PoisonTopic == "" {
		return errors.New("eventbus: consumer needs a poison publisher and topic")
	}
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = 3
	}

	poison, err := middleware.PoisonQueueWithFilter(opts.Poison, opts.PoisonTopic, func(err error) bool {
		return !errors.Is(err, ErrDrop)
	})
	if err != nil {
		return err
	}
	retry := middleware.Retry{
		MaxRetries:      opts.MaxRetries,
		InitialInterval: 100 * time.Millisecond,
		MaxInterval:     5 * time.Second,
		Multiplier:      2,
		Logger:          r.Logger(),
	}.Middleware

	for _, s := range subs {
		h := s.Handler
		handler := r.AddConsumerHandler(s.Topic, s.Topic, sub, func(msg *message.Message) error {
			return h.Handle(msg.Context(), msg.Payload)
		})
		handler.AddMiddleware(poison, retry, drop)
	}

	return nil
}

// ProvidePublisher is the outbox every BC publishes through: watermill-sql
// rows in the owning BC's schema (TableName), written inside the use case's
// transaction, or GoChannel (in-process, at most once) when there is no
// database. Both carry trace context. Apps provide it once (wire.go) and every
// BC's events adapter shares it.
func ProvidePublisher(db *gorm.DB, l *slog.Logger, tel *otel.Providers) (message.Publisher, func(), error) {
	if db == nil {
		ch := NewGoChannel(l)
		return Traced(ch, tel), func() { _ = ch.Close() }, nil
	}

	pub, err := NewPostgresPublisher(db, l)
	if err != nil {
		return nil, nil, err
	}

	return Traced(pub, tel), func() {}, nil
}

const scopeName = "github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"

// drop turns ErrDrop into an ack with a log line; everything else passes
// through to Retry / PoisonQueue.
func drop(h message.HandlerFunc) message.HandlerFunc {
	return func(msg *message.Message) ([]*message.Message, error) {
		out, err := h(msg)
		if errors.Is(err, ErrDrop) {
			contextx.FromContext(msg.Context()).Info("eventbus: message dropped by handler", "message_id", msg.UUID, "error", err)
			return out, nil
		}

		return out, err
	}
}

type tracing struct {
	propagator propagation.TextMapPropagator
	tracer     trace.Tracer
}

func (t tracing) middleware(h message.HandlerFunc) message.HandlerFunc {
	return func(msg *message.Message) ([]*message.Message, error) {
		topic := message.SubscribeTopicFromCtx(msg.Context())
		parent := t.propagator.Extract(msg.Context(), propagation.MapCarrier(msg.Metadata))
		ctx, span := t.tracer.Start(parent, topic+" process",
			trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithAttributes(
				attribute.String("messaging.system", "watermill"),
				attribute.String("messaging.destination.name", topic),
				attribute.String("messaging.message.id", msg.UUID),
				attribute.String("messaging.consumer.group.name", message.HandlerNameFromCtx(msg.Context())),
			),
		)
		defer span.End()
		msg.SetContext(ctx)

		out, err := h(msg)
		if err != nil && !errors.Is(err, ErrDrop) {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}

		return out, err
	}
}

// Traced decorates a publisher so every message carries the publisher's
// trace context (W3C traceparent in metadata) and a PRODUCER span.
func Traced(pub message.Publisher, tel *otel.Providers) message.Publisher {
	return &tracedPublisher{Publisher: pub, propagator: tel.Propagator, tracer: tel.Tracer.Tracer(scopeName)}
}

type tracedPublisher struct {
	message.Publisher
	propagator propagation.TextMapPropagator
	tracer     trace.Tracer
}

func (p *tracedPublisher) Publish(topic string, msgs ...*message.Message) error {
	for _, msg := range msgs {
		ctx, span := p.tracer.Start(msg.Context(), topic+" publish",
			trace.WithSpanKind(trace.SpanKindProducer),
			trace.WithAttributes(
				attribute.String("messaging.system", "watermill"),
				attribute.String("messaging.destination.name", topic),
				attribute.String("messaging.message.id", msg.UUID),
			),
		)
		if msg.Metadata == nil {
			msg.Metadata = message.Metadata{}
		}
		p.propagator.Inject(ctx, propagation.MapCarrier(msg.Metadata))
		msg.SetContext(ctx)
		span.End()
	}
	if err := p.Publisher.Publish(topic, msgs...); err != nil {
		return fmt.Errorf("publish %s: %w", topic, err)
	}

	return nil
}
