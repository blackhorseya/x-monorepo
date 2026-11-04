package events_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/events"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// calls is the shared log of what happened, in order: the proof that each event is published and
// then recorded, one after the other, in the same call.
type calls struct{ log []string }

type nextPub struct {
	calls *calls
	err   error
}

func (p nextPub) Publish(_ context.Context, evs ...order.Event) error {
	for _, e := range evs {
		p.calls.log = append(p.calls.log, "publish "+e.To().String())
	}

	return p.err
}

type fakeStore struct {
	calls   *calls
	err     error
	entries []order.HistoryEntry
}

func (s *fakeStore) Append(_ context.Context, es ...order.HistoryEntry) error {
	for _, e := range es {
		s.calls.log = append(s.calls.log, "append "+e.To().String())
	}
	if s.err == nil {
		s.entries = append(s.entries, es...)
	}

	return s.err
}

func (s *fakeStore) List(context.Context, order.ID) ([]order.HistoryEntry, error) { return nil, nil }

var _ port.HistoryStore = (*fakeStore)(nil)

func capturedEvents(t *testing.T) []order.Event {
	t.Helper()
	usd, _ := kernel.NewMoney(100, "USD")
	it, _ := order.NewLineItem("S", 1, usd)
	at := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	o, err := order.Reconstitute(order.ReconstituteParams{ID: "o-1", CustomerID: "c-1", Items: []order.LineItem{it}, Status: order.StatusConfirmed, PaymentRequired: true, CreatedAt: at, UpdatedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.ApplyPaymentCaptured(at); err != nil {
		t.Fatal(err)
	}

	return o.PullEvents()
}

func TestRecorder_PublishesThenRecordsEachEvent(t *testing.T) {
	cl := &calls{}
	store := &fakeStore{calls: cl}
	r := events.NewRecorder(nextPub{calls: cl}, store, "admin")

	if err := r.Publish(context.Background(), capturedEvents(t)...); err != nil {
		t.Fatal(err)
	}
	wantLog := []string{"publish PAID", "append PAID", "publish COMPLETED", "append COMPLETED"}
	if !reflect.DeepEqual(cl.log, wantLog) {
		t.Fatalf("call order = %v, want %v", cl.log, wantLog)
	}
	if len(store.entries) != 2 || store.entries[0].Seq() != 0 || store.entries[1].Seq() != 0 {
		t.Fatalf("entries = %+v; the recorder leaves seq to the store", store.entries)
	}
}

func TestRecorder_NothingToPublishTouchesNothing(t *testing.T) {
	cl := &calls{}
	store := &fakeStore{calls: cl}
	if err := events.NewRecorder(nextPub{calls: cl}, store, "admin").Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(cl.log) != 0 {
		t.Fatalf("calls = %v, want none", cl.log)
	}
}

func TestRecorder_ErrorsAreNotSwallowed(t *testing.T) {
	evs := capturedEvents(t)

	pubErr := errors.New("outbox down")
	cl := &calls{}
	store := &fakeStore{calls: cl}
	if err := events.NewRecorder(nextPub{calls: cl, err: pubErr}, store, "admin").Publish(context.Background(), evs...); !errors.Is(err, pubErr) {
		t.Fatalf("publish failure err = %v, want %v", err, pubErr)
	}
	if len(store.entries) != 0 {
		t.Fatalf("a failed publish still recorded %d rows", len(store.entries))
	}

	storeErr := errors.New("history down")
	cl2 := &calls{}
	if err := events.NewRecorder(nextPub{calls: cl2}, &fakeStore{calls: cl2, err: storeErr}, "admin").Publish(context.Background(), evs...); !errors.Is(err, storeErr) {
		t.Fatalf("append failure err = %v, want %v", err, storeErr)
	}
	if !reflect.DeepEqual(cl2.log, []string{"publish PAID", "append PAID"}) {
		t.Fatalf("calls after the first append failed = %v; it must stop there", cl2.log)
	}
}

// Who did it is decided from the principal for an rpc-caused transition, and is SYSTEM for the
// ones caused by an event — whatever the context carries.
func TestRecorder_ActorMatrix(t *testing.T) {
	at := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	usd, _ := kernel.NewMoney(100, "USD")
	it, _ := order.NewLineItem("S", 1, usd)
	newOrder := func() *order.Order {
		o, err := order.Reconstitute(order.ReconstituteParams{ID: "o-1", CustomerID: "c-1", Items: []order.LineItem{it}, Status: order.StatusPending, CreatedAt: at, UpdatedAt: at})
		if err != nil {
			t.Fatal(err)
		}

		return o
	}
	rpcEvent := func() order.Event {
		o := newOrder()
		if err := o.Cancel(at, order.CauseRPCRequest); err != nil {
			t.Fatal(err)
		}

		return o.PullEvents()[0]
	}
	eventEvent := func() order.Event {
		o := newOrder()
		if err := o.Cancel(at, order.CauseCancellationRequested); err != nil {
			t.Fatal(err)
		}

		return o.PullEvents()[0]
	}

	cases := []struct {
		name        string
		principal   *contextx.Principal
		event       order.Event
		wantKind    order.ActorKind
		wantSubject string
	}{
		{"rpc, customer", &contextx.Principal{Subject: "acc-1", Roles: []string{"customer"}}, rpcEvent(), order.ActorCustomer, "acc-1"},
		{"rpc, no roles at all", &contextx.Principal{Subject: "acc-1"}, rpcEvent(), order.ActorCustomer, "acc-1"},
		{"rpc, admin", &contextx.Principal{Subject: "acc-9", Roles: []string{"customer", "ops-admin"}}, rpcEvent(), order.ActorAdmin, "acc-9"},
		{"rpc, a role that merely contains the admin role", &contextx.Principal{Subject: "acc-1", Roles: []string{"ops-admin-x"}}, rpcEvent(), order.ActorCustomer, "acc-1"},
		{"rpc, anonymous (auth.mode=off)", nil, rpcEvent(), order.ActorAnonymous, ""},
		{"event, no principal", nil, eventEvent(), order.ActorSystem, ""},
		{"event, even with a principal in the context", &contextx.Principal{Subject: "acc-9", Roles: []string{"ops-admin"}}, eventEvent(), order.ActorSystem, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := context.Background()
			if tc.principal != nil {
				c = contextx.WithPrincipal(c, *tc.principal)
			}
			cl := &calls{}
			store := &fakeStore{calls: cl}
			if err := events.NewRecorder(nextPub{calls: cl}, store, "ops-admin").Publish(c, tc.event); err != nil {
				t.Fatal(err)
			}
			if len(store.entries) != 1 {
				t.Fatalf("%d entries", len(store.entries))
			}
			got := store.entries[0]
			if got.ActorKind() != tc.wantKind || got.ActorSubject() != tc.wantSubject {
				t.Fatalf("actor = %s %q, want %s %q", got.ActorKind(), got.ActorSubject(), tc.wantKind, tc.wantSubject)
			}
			if strings.Contains(got.Cause().String(), " ") {
				t.Fatalf("cause %q is not a fixed enumeration value", got.Cause())
			}
		})
	}
}
