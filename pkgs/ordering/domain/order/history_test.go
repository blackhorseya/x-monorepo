package order_test

import (
	"errors"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
)

func historyItem(t *testing.T) []order.LineItem {
	t.Helper()
	usd, _ := kernel.NewMoney(500, "USD")
	it, err := order.NewLineItem("S", 1, usd)
	if err != nil {
		t.Fatal(err)
	}

	return []order.LineItem{it}
}

func orderAt(t *testing.T, status order.Status, paymentRequired bool) *order.Order {
	t.Helper()
	at := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	o, err := order.Reconstitute(order.ReconstituteParams{ID: "o-1", CustomerID: "c-1", Items: historyItem(t), Status: status, PaymentRequired: paymentRequired, CreatedAt: at, UpdatedAt: at})
	if err != nil {
		t.Fatal(err)
	}

	return o
}

// Every transition raises an event that says where it came from, where it went and why, so the
// history recorder needs nothing but the event (#427 decision 2/4).
func TestEvents_CarryFromToAndCause(t *testing.T) {
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	type step struct {
		name  string
		run   func(t *testing.T) []order.Event
		wants []struct {
			from, to order.Status
			cause    order.Cause
		}
	}
	type w = struct {
		from, to order.Status
		cause    order.Cause
	}
	steps := []step{
		{"New", func(t *testing.T) []order.Event {
			o, err := order.New(order.Params{ID: "o-1", CustomerID: "c-1", Items: historyItem(t), Now: now})
			if err != nil {
				t.Fatal(err)
			}

			return o.PullEvents()
		}, []w{{order.StatusUnknown, order.StatusPending, order.CauseRPCRequest}}},
		{"Confirm", func(t *testing.T) []order.Event {
			o := orderAt(t, order.StatusPending, false)
			if err := o.Confirm(now, false); err != nil {
				t.Fatal(err)
			}

			return o.PullEvents()
		}, []w{{order.StatusPending, order.StatusConfirmed, order.CauseStockReserved}}},
		{"Complete from CONFIRMED", func(t *testing.T) []order.Event {
			o := orderAt(t, order.StatusConfirmed, false)
			if err := o.Complete(now); err != nil {
				t.Fatal(err)
			}

			return o.PullEvents()
		}, []w{{order.StatusConfirmed, order.StatusCompleted, order.CauseRPCRequest}}},
		{"Complete from PAID", func(t *testing.T) []order.Event {
			o := orderAt(t, order.StatusPaid, true)
			if err := o.Complete(now); err != nil {
				t.Fatal(err)
			}

			return o.PullEvents()
		}, []w{{order.StatusPaid, order.StatusCompleted, order.CauseRPCRequest}}},
		{"Cancel from PENDING by rpc", func(t *testing.T) []order.Event {
			o := orderAt(t, order.StatusPending, false)
			if err := o.Cancel(now, order.CauseRPCRequest); err != nil {
				t.Fatal(err)
			}

			return o.PullEvents()
		}, []w{{order.StatusPending, order.StatusCancelled, order.CauseRPCRequest}}},
		{"Cancel from CONFIRMED by request", func(t *testing.T) []order.Event {
			o := orderAt(t, order.StatusConfirmed, false)
			if err := o.Cancel(now, order.CauseCancellationRequested); err != nil {
				t.Fatal(err)
			}

			return o.PullEvents()
		}, []w{{order.StatusConfirmed, order.StatusCancelled, order.CauseCancellationRequested}}},
		{"MarkPaid", func(t *testing.T) []order.Event {
			o := orderAt(t, order.StatusConfirmed, true)
			if err := o.MarkPaid(now); err != nil {
				t.Fatal(err)
			}

			return o.PullEvents()
		}, []w{{order.StatusConfirmed, order.StatusPaid, order.CausePaymentCaptured}}},
		{"ApplyPaymentCaptured", func(t *testing.T) []order.Event {
			o := orderAt(t, order.StatusConfirmed, true)
			if eff, err := o.ApplyPaymentCaptured(now); err != nil || eff != order.PaymentApplied {
				t.Fatalf("apply = %v, %v", eff, err)
			}

			return o.PullEvents()
		}, []w{{order.StatusConfirmed, order.StatusPaid, order.CausePaymentCaptured}, {order.StatusPaid, order.StatusCompleted, order.CausePaymentCaptured}}},
		{"ApplyPaymentExpired", func(t *testing.T) []order.Event {
			o := orderAt(t, order.StatusConfirmed, true)
			if eff, err := o.ApplyPaymentExpired(now); err != nil || eff != order.PaymentApplied {
				t.Fatalf("apply = %v, %v", eff, err)
			}

			return o.PullEvents()
		}, []w{{order.StatusConfirmed, order.StatusCancelled, order.CausePaymentExpired}}},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			evs := s.run(t)
			if len(evs) != len(s.wants) {
				t.Fatalf("%d events, want %d", len(evs), len(s.wants))
			}
			for n, e := range evs {
				if e.From() != s.wants[n].from || e.To() != s.wants[n].to || e.Cause() != s.wants[n].cause {
					t.Errorf("event %d (%T) = %s→%s by %s, want %s→%s by %s", n, e, e.From(), e.To(), e.Cause(), s.wants[n].from, s.wants[n].to, s.wants[n].cause)
				}
			}
		})
	}
}

// Cancel takes its cause from the caller, and only the two causes a cancel can have are accepted.
// The zero value is refused, not defaulted: a caller that forgot to say why gets an error and the
// order stays where it was.
func TestCancel_CauseIsValidated(t *testing.T) {
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	for _, cause := range []order.Cause{order.CauseUnspecified, order.CauseStockReserved, order.CausePaymentCaptured, order.Cause(200)} {
		o := orderAt(t, order.StatusConfirmed, false)
		if err := o.Cancel(now, cause); !errors.Is(err, order.ErrInvalidCause) {
			t.Fatalf("Cancel(%s) err = %v, want ErrInvalidCause", cause, err)
		}
		if o.Status() != order.StatusConfirmed || len(o.PullEvents()) != 0 {
			t.Fatalf("Cancel(%s) changed the order (status %s)", cause, o.Status())
		}
	}
	// A refusal by the state machine still comes first for a valid cause.
	done := orderAt(t, order.StatusCompleted, false)
	if err := done.Cancel(now, order.CauseRPCRequest); !errors.Is(err, order.ErrCannotCancel) {
		t.Fatalf("cancel completed err = %v, want ErrCannotCancel", err)
	}
}

func TestCause_StringAndParse(t *testing.T) {
	want := map[order.Cause]string{
		order.CauseUnspecified:           "UNSPECIFIED",
		order.CauseRPCRequest:            "RPC_REQUEST",
		order.CausePaymentCaptured:       "PAYMENT_CAPTURED",
		order.CausePaymentExpired:        "PAYMENT_EXPIRED",
		order.CauseStockReserved:         "STOCK_RESERVED",
		order.CauseCancellationRequested: "CANCELLATION_REQUESTED",
	}
	for cause, s := range want {
		if cause.String() != s {
			t.Errorf("%d.String() = %q, want %q", cause, cause.String(), s)
		}
		got, err := order.ParseCause(s)
		if err != nil || got != cause {
			t.Errorf("ParseCause(%q) = %v, %v", s, got, err)
		}
	}
	if order.Cause(200).String() != "UNSPECIFIED" {
		t.Errorf("unknown cause renders as %q", order.Cause(200).String())
	}
	// The free text of another BC is never a cause: anything not in the table is refused.
	for _, bad := range []string{"", "rpc_request", "reservation r-1 expired after 24h", "stock_shortage"} {
		if _, err := order.ParseCause(bad); !errors.Is(err, order.ErrInvalidCause) {
			t.Errorf("ParseCause(%q) err = %v, want ErrInvalidCause", bad, err)
		}
	}
}

func TestActorKind_StringAndParse(t *testing.T) {
	want := map[order.ActorKind]string{
		order.ActorCustomer:  "CUSTOMER",
		order.ActorAdmin:     "ADMIN",
		order.ActorSystem:    "SYSTEM",
		order.ActorAnonymous: "ANONYMOUS",
	}
	for kind, s := range want {
		if kind.String() != s {
			t.Errorf("%d.String() = %q, want %q", kind, kind.String(), s)
		}
		got, err := order.ParseActorKind(s)
		if err != nil || got != kind {
			t.Errorf("ParseActorKind(%q) = %v, %v", s, got, err)
		}
	}
	if order.ActorUnspecified.String() != "UNSPECIFIED" {
		t.Errorf("zero actor kind renders as %q", order.ActorUnspecified.String())
	}
	for _, bad := range []string{"", "UNSPECIFIED", "customer", "ROOT"} {
		if _, err := order.ParseActorKind(bad); !errors.Is(err, order.ErrInvalidActor) {
			t.Errorf("ParseActorKind(%q) err = %v, want ErrInvalidActor", bad, err)
		}
	}
}

func TestHistoryEntry_FromEvent(t *testing.T) {
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	o := orderAt(t, order.StatusConfirmed, true)
	if _, err := o.ApplyPaymentCaptured(now); err != nil {
		t.Fatal(err)
	}
	evs := o.PullEvents()
	if len(evs) != 2 {
		t.Fatalf("%d events, want 2", len(evs))
	}

	names := []string{"PAID", "COMPLETED"}
	for n, e := range evs {
		h, err := order.NewHistoryEntry(e, order.ActorSystem, "")
		if err != nil {
			t.Fatal(err)
		}
		if h.OrderID() != "o-1" || h.Seq() != 0 || h.Event() != names[n] || h.From() != e.From() || h.To() != e.To() ||
			!h.OccurredAt().Equal(now) || h.ActorKind() != order.ActorSystem || h.ActorSubject() != "" || h.Cause() != order.CausePaymentCaptured {
			t.Errorf("entry %d = %+v", n, h)
		}
	}

	// A subject without a person behind it is not stored, and an actor must be said.
	if _, err := order.NewHistoryEntry(evs[0], order.ActorUnspecified, ""); !errors.Is(err, order.ErrInvalidActor) {
		t.Errorf("unspecified actor err = %v", err)
	}
	if _, err := order.NewHistoryEntry(evs[0], order.ActorSystem, "acc-1"); !errors.Is(err, order.ErrInvalidActor) {
		t.Errorf("SYSTEM with a subject err = %v, want ErrInvalidActor", err)
	}
	if _, err := order.NewHistoryEntry(evs[0], order.ActorAnonymous, "acc-1"); !errors.Is(err, order.ErrInvalidActor) {
		t.Errorf("ANONYMOUS with a subject err = %v, want ErrInvalidActor", err)
	}
	if _, err := order.NewHistoryEntry(evs[0], order.ActorCustomer, ""); !errors.Is(err, order.ErrInvalidActor) {
		t.Errorf("CUSTOMER without a subject err = %v, want ErrInvalidActor", err)
	}
}

func TestHistoryEntry_EventNames(t *testing.T) {
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	created, _ := order.New(order.Params{ID: "o-1", CustomerID: "c-1", Items: historyItem(t), Now: now})
	confirmed := orderAt(t, order.StatusPending, false)
	_ = confirmed.Confirm(now, false)
	cancelled := orderAt(t, order.StatusPending, false)
	_ = cancelled.Cancel(now, order.CauseRPCRequest)

	for _, tc := range []struct {
		e    order.Event
		want string
	}{
		{created.PullEvents()[0], "CREATED"},
		{confirmed.PullEvents()[0], "CONFIRMED"},
		{cancelled.PullEvents()[0], "CANCELLED"},
	} {
		h, err := order.NewHistoryEntry(tc.e, order.ActorAdmin, "acc-1")
		if err != nil || h.Event() != tc.want {
			t.Errorf("entry for %T = %q, %v; want %q", tc.e, h.Event(), err, tc.want)
		}
	}
}

func TestReconstituteHistoryEntry(t *testing.T) {
	at := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	p := order.HistoryParams{
		OrderID: "o-1", Seq: 2, From: order.StatusPending, To: order.StatusConfirmed, OccurredAt: at,
		ActorKind: order.ActorSystem, Cause: order.CauseStockReserved,
	}
	h, err := order.ReconstituteHistoryEntry(p)
	if err != nil {
		t.Fatal(err)
	}
	if h.OrderID() != "o-1" || h.Seq() != 2 || h.Event() != "CONFIRMED" || h.From() != order.StatusPending || h.To() != order.StatusConfirmed ||
		!h.OccurredAt().Equal(at) || h.ActorKind() != order.ActorSystem || h.Cause() != order.CauseStockReserved {
		t.Fatalf("entry = %+v", h)
	}

	bad := map[string]func(*order.HistoryParams){
		"empty order id":    func(p *order.HistoryParams) { p.OrderID = "" },
		"zero seq":          func(p *order.HistoryParams) { p.Seq = 0 },
		"unknown to status": func(p *order.HistoryParams) { p.To = order.StatusUnknown },
		"unspecified actor": func(p *order.HistoryParams) { p.ActorKind = order.ActorUnspecified },
		"zero time":         func(p *order.HistoryParams) { p.OccurredAt = time.Time{} },
	}
	for name, mutate := range bad {
		q := p
		mutate(&q)
		if _, err := order.ReconstituteHistoryEntry(q); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
