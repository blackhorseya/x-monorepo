package eventbus_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
)

func TestNaming(t *testing.T) {
	if got := eventbus.TableName("ordering.v1.OrderCreated"); got != "ordering.events_v1_ordercreated" {
		t.Fatalf("TableName = %s", got)
	}
	if got := eventbus.OffsetsTableName("ordering.v1.OrderCreated"); got != "ordering.events_v1_ordercreated_offsets" {
		t.Fatalf("OffsetsTableName = %s", got)
	}
	if got := eventbus.PoisonTopic("ordering"); got != "ordering.poison" || eventbus.TableName(got) != "ordering.events_poison" {
		t.Fatalf("PoisonTopic = %s → %s", got, eventbus.TableName(got))
	}
}

// capture is a message.Publisher that records what it was given (the poison sink).
type capture struct {
	mu   sync.Mutex
	msgs []*message.Message
}

func (c *capture) Publish(_ string, msgs ...*message.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, msgs...)
	return nil
}
func (c *capture) Close() error { return nil }
func (c *capture) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.msgs)
}

// The whole middleware stack on GoChannel: ErrDrop is acked and never
// poisoned, a persistent error is retried then parked, and the consumer span
// joins the producer's trace.
func TestRouter_ErrorContractAndTracing(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	logger := slog.New(slog.NewTextHandler(discard{}, &slog.HandlerOptions{Level: slog.LevelError}))
	tel := &otel.Providers{Tracer: tp, Propagator: propagation.TraceContext{}, Logger: logger}

	ch := eventbus.NewGoChannel(logger)
	defer func() { _ = ch.Close() }()
	pub := eventbus.Traced(ch, tel)
	poison := &capture{}

	router, err := eventbus.NewRouter(logger, tel)
	if err != nil {
		t.Fatal(err)
	}

	var dropCalls, failCalls atomic.Int32
	var seen trace.SpanContext
	okDone := make(chan struct{}, 1)
	err = eventbus.Register(router, ch, eventbus.ConsumerOptions{Poison: poison, PoisonTopic: "t.poison", MaxRetries: 1},
		eventbus.Subscription{Topic: "t.ok", Handler: eventbus.HandlerFunc(func(c context.Context, _ []byte) error {
			seen = trace.SpanContextFromContext(c)
			okDone <- struct{}{}
			return nil
		})},
		eventbus.Subscription{Topic: "t.drop", Handler: eventbus.HandlerFunc(func(context.Context, []byte) error {
			dropCalls.Add(1)
			return errors.Join(eventbus.ErrDrop, errors.New("undecodable"))
		})},
		eventbus.Subscription{Topic: "t.fail", Handler: eventbus.HandlerFunc(func(context.Context, []byte) error {
			failCalls.Add(1)
			return errors.New("db down")
		})},
	)
	if err != nil {
		t.Fatal(err)
	}

	c, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- router.Run(c) }()
	<-router.Running()

	// Producer span → metadata → consumer span.
	producerCtx, producer := tp.Tracer("test").Start(context.Background(), "rpc")
	for _, topic := range []string{"t.ok", "t.drop", "t.fail"} {
		if err := pub.Publish(topic, message.NewMessageWithContext(producerCtx, watermill.NewUUID(), []byte(topic))); err != nil {
			t.Fatal(err)
		}
	}
	producer.End()

	select {
	case <-okDone:
	case <-time.After(5 * time.Second):
		t.Fatal("t.ok not delivered")
	}
	if seen.TraceID() != producer.SpanContext().TraceID() {
		t.Fatalf("consumer trace %s != producer trace %s", seen.TraceID(), producer.SpanContext().TraceID())
	}

	deadline := time.Now().Add(5 * time.Second)
	for poison.len() < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("t.fail never reached the poison queue (fail calls = %d)", failCalls.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // any redelivery of t.drop would show up here
	if string(poison.msgs[0].Payload) != "t.fail" || poison.msgs[0].Metadata.Get("reason_poisoned") != "db down" {
		t.Fatalf("poisoned = %q meta=%v", poison.msgs[0].Payload, poison.msgs[0].Metadata)
	}
	if failCalls.Load() != 2 { // first attempt + MaxRetries
		t.Fatalf("fail calls = %d, want 2", failCalls.Load())
	}
	if dropCalls.Load() != 1 || poison.len() != 1 {
		t.Fatalf("drop calls = %d poison = %d; ErrDrop must ack without retry or poison", dropCalls.Load(), poison.len())
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	var kinds []trace.SpanKind
	for _, s := range exp.GetSpans() {
		if s.Name == "t.ok publish" || s.Name == "t.ok process" {
			kinds = append(kinds, s.SpanKind)
		}
	}
	if len(kinds) != 2 || kinds[0] != trace.SpanKindProducer || kinds[1] != trace.SpanKindConsumer {
		t.Fatalf("t.ok spans = %v, want producer then consumer", kinds)
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
