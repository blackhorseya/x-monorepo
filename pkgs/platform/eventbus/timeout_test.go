package eventbus_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
)

// HandlerTimeout: a handler that outlives the deadline fails that attempt
// with ErrHandlerTimeout, is retried with a fresh deadline each time (the
// parent context is restored, otherwise Retry would see Done() and give up
// at once) and is parked with that reason. ErrDrop after the deadline is
// still a drop, a late success is still an ack, and 0 means no deadline.
func TestRegister_HandlerTimeout(t *testing.T) {
	logs := &buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	tel := &otel.Providers{Tracer: sdktrace.NewTracerProvider(), Propagator: propagation.TraceContext{}, Logger: logger}

	ch := eventbus.NewGoChannel(logger)
	defer func() { _ = ch.Close() }()
	poison := &capture{}
	router, err := eventbus.NewRouter(logger, tel)
	if err != nil {
		t.Fatal(err)
	}

	const timeout = 50 * time.Millisecond
	var hangCalls, dropCalls atomic.Int32
	var hangErr atomic.Value
	var shortest atomic.Int64 // ns of the quickest t.hang attempt
	shortest.Store(int64(time.Hour))
	lateDone := make(chan struct{}, 1)
	err = eventbus.Register(router, ch, eventbus.ConsumerOptions{Poison: poison, PoisonTopic: "t.poison", MaxRetries: 2, HandlerTimeout: timeout},
		eventbus.Subscription{Topic: "t.hang", Handler: eventbus.HandlerFunc(func(c context.Context, _ []byte) error {
			hangCalls.Add(1)
			if _, ok := c.Deadline(); !ok {
				t.Error("handler context has no deadline")
			}
			began := time.Now()
			<-c.Done()
			if d := int64(time.Since(began)); d < shortest.Load() {
				shortest.Store(d)
			}
			hangErr.Store(c.Err())
			return c.Err()
		})},
		eventbus.Subscription{Topic: "t.drop", Handler: eventbus.HandlerFunc(func(c context.Context, _ []byte) error {
			dropCalls.Add(1)
			<-c.Done()
			return errors.Join(eventbus.ErrDrop, c.Err())
		})},
		eventbus.Subscription{Topic: "t.late", Handler: eventbus.HandlerFunc(func(c context.Context, _ []byte) error {
			<-c.Done() // the work was committed just before the deadline: success stands
			lateDone <- struct{}{}
			return nil
		})},
	)
	if err != nil {
		t.Fatal(err)
	}
	// A second BC without a timeout on the same router: its handlers see no deadline.
	noDeadline := make(chan bool, 1)
	err = eventbus.Register(router, ch, eventbus.ConsumerOptions{Poison: poison, PoisonTopic: "u.poison", MaxRetries: 1},
		eventbus.Subscription{Topic: "u.free", Handler: eventbus.HandlerFunc(func(c context.Context, _ []byte) error {
			_, ok := c.Deadline()
			noDeadline <- !ok
			return nil
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

	for _, topic := range []string{"t.hang", "t.drop", "t.late", "u.free"} {
		if err := ch.Publish(topic, message.NewMessage(watermill.NewUUID(), []byte(topic))); err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for poison.len() < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("t.hang never reached the poison queue (calls = %d)", hangCalls.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 3 attempts of ≥ timeout each: the retries really happened and each had
	// its own deadline, not a cancelled parent.
	if got := hangCalls.Load(); got != 3 {
		t.Fatalf("hang attempts = %d, want 3 (first + MaxRetries)", got)
	}
	// Every attempt waited for its own deadline: a retry whose deadline
	// derived from the previous attempt's expired context would return at once.
	if got := time.Duration(shortest.Load()); got < timeout*9/10 {
		t.Fatalf("shortest hang attempt = %s, want ≈ %s (each retry must get a fresh deadline)", got, timeout)
	}
	if !errors.Is(hangErr.Load().(error), context.DeadlineExceeded) {
		t.Fatalf("handler saw %v, want DeadlineExceeded", hangErr.Load())
	}
	parked := poison.msgs[0]
	reason := parked.Metadata.Get("reason_poisoned")
	if string(parked.Payload) != "t.hang" || !strings.Contains(reason, "handler timed out after "+timeout.String()) || !strings.Contains(reason, "deadline exceeded") {
		t.Fatalf("poisoned = %q reason=%q", parked.Payload, reason)
	}

	select {
	case <-lateDone:
	case <-time.After(5 * time.Second):
		t.Fatal("t.late not delivered")
	}
	select {
	case free := <-noDeadline:
		if !free {
			t.Fatal("u.free got a deadline from the other BC's options")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("u.free not delivered")
	}
	time.Sleep(300 * time.Millisecond) // a retry of t.drop / t.late would show up here
	if dropCalls.Load() != 1 || poison.len() != 1 {
		t.Fatalf("drop calls = %d poison = %d; ErrDrop after the deadline must still drop", dropCalls.Load(), poison.len())
	}
	if got := logs.count("msg=message", "topic=t.hang ", "result=failed", "handler timed out"); got != 3 {
		t.Fatalf("failed access lines for t.hang: %d, want 3\n%s", got, logs.all())
	}
	if got := logs.count("msg=message", "topic=t.late ", "result=ok"); got != 1 {
		t.Fatalf("ok access lines for t.late: %d, want 1\n%s", got, logs.all())
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
