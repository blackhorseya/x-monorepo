package consumer_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/adapters/consumer"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/stock"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
)

// stub lets the test choose the use case outcome; the handler's job is the
// decode step and the drop / retry decision.
type stub struct {
	in  any
	out *dto.ReservationOutput
	err error
}

func (s *stub) Execute(_ context.Context, in any) (*dto.ReservationOutput, error) {
	s.in = in
	return s.out, s.err
}

type reserveStub struct{ stub }

func (s *reserveStub) Execute(c context.Context, in dto.ReserveStockInput) (*dto.ReservationOutput, error) {
	return s.stub.Execute(c, in)
}

type releaseStub struct{ stub }

func (s *releaseStub) Execute(c context.Context, in dto.ReleaseStockInput) (*dto.ReservationOutput, error) {
	return s.stub.Execute(c, in)
}

type commitStub struct{ stub }

func (s *commitStub) Execute(c context.Context, in dto.CommitStockInput) (*dto.ReservationOutput, error) {
	return s.stub.Execute(c, in)
}

func TestOrderCompletedHandler(t *testing.T) {
	c := context.Background()
	uc := &commitStub{}
	h := consumer.NewOrderCompletedHandler(uc)
	payload, _ := proto.Marshal(&orderingv1.OrderCompleted{OrderId: "o-1"})

	uc.out = &dto.ReservationOutput{OrderID: "o-1", Status: "COMMITTED"}
	if err := h.Handle(c, payload); err != nil {
		t.Fatal(err)
	}
	if in, ok := uc.in.(dto.CommitStockInput); !ok || in.OrderID != "o-1" {
		t.Fatalf("input = %#v", uc.in)
	}
	// Already released / committed, or no reservation at all: final.
	uc.out, uc.err = nil, reservation.ErrNotReserved
	if err := h.Handle(c, payload); !errors.Is(err, consumer.ErrDrop) {
		t.Fatalf("not reserved: %v", err)
	}
	uc.err = reservation.ErrNotFound
	if err := h.Handle(c, payload); !errors.Is(err, consumer.ErrDrop) {
		t.Fatalf("not found: %v", err)
	}
	uc.err = errors.New("db down")
	if err := h.Handle(c, payload); errors.Is(err, consumer.ErrDrop) {
		t.Fatalf("infra error must retry: %v", err)
	}
	if err := h.Handle(c, []byte{0xff, 0xff}); !errors.Is(err, consumer.ErrDrop) {
		t.Fatalf("garbage: %v", err)
	}
}

func TestOrderCreatedHandler(t *testing.T) {
	c := context.Background()
	uc := &reserveStub{}
	h := consumer.NewOrderCreatedHandler(uc)
	payload, _ := proto.Marshal(&orderingv1.OrderCreated{
		OrderId: "o-1",
		Items:   []*orderingv1.OrderCreatedItem{{Sku: "A", Quantity: 2}, {Sku: "B", Quantity: 1}},
	})

	// ok, and a shortfall is still ok (the use case already published the request).
	for _, out := range []*dto.ReservationOutput{{OrderID: "o-1", Status: "RESERVED"}, {OrderID: "o-1", Status: "REJECTED", ShortfallSKUs: []string{"B"}}} {
		uc.out, uc.err = out, nil
		if err := h.Handle(c, payload); err != nil {
			t.Fatalf("%s: %v", out.Status, err)
		}
	}
	in, ok := uc.in.(dto.ReserveStockInput)
	if !ok || in.OrderID != "o-1" || len(in.Lines) != 2 || in.Lines[1].SKU != "B" || in.Lines[0].Quantity != 2 {
		t.Fatalf("input = %#v", uc.in)
	}

	// Domain error → drop; infrastructure error (incl. a lost optimistic lock) → retry.
	uc.out, uc.err = nil, reservation.ErrDuplicate
	if err := h.Handle(c, payload); !errors.Is(err, consumer.ErrDrop) || !errors.Is(err, reservation.ErrDuplicate) {
		t.Fatalf("domain error: %v", err)
	}
	uc.err = stock.ErrStale
	if err := h.Handle(c, payload); errors.Is(err, consumer.ErrDrop) || !errors.Is(err, stock.ErrStale) {
		t.Fatalf("stale: %v", err)
	}
	uc.err = errors.New("db down")
	if err := h.Handle(c, payload); errors.Is(err, consumer.ErrDrop) {
		t.Fatalf("infra error must retry: %v", err)
	}
	if err := h.Handle(c, []byte{0xff, 0xff}); !errors.Is(err, consumer.ErrDrop) {
		t.Fatalf("garbage: %v", err)
	}
}

func TestOrderCancelledHandler(t *testing.T) {
	c := context.Background()
	uc := &releaseStub{}
	h := consumer.NewOrderCancelledHandler(uc)
	payload, _ := proto.Marshal(&orderingv1.OrderCancelled{OrderId: "o-1"})

	uc.out = &dto.ReservationOutput{OrderID: "o-1", Status: "RELEASED"}
	if err := h.Handle(c, payload); err != nil {
		t.Fatal(err)
	}
	if in, ok := uc.in.(dto.ReleaseStockInput); !ok || in.OrderID != "o-1" {
		t.Fatalf("input = %#v", uc.in)
	}
	// Order cancelled before inventory ever saw it (or rejected by inventory itself): final.
	uc.out, uc.err = nil, reservation.ErrNotFound
	if err := h.Handle(c, payload); !errors.Is(err, consumer.ErrDrop) {
		t.Fatalf("not found: %v", err)
	}
	uc.err = reservation.ErrNotReserved
	if err := h.Handle(c, payload); !errors.Is(err, consumer.ErrDrop) {
		t.Fatalf("not reserved: %v", err)
	}
	uc.err = errors.New("db down")
	if err := h.Handle(c, payload); errors.Is(err, consumer.ErrDrop) {
		t.Fatalf("infra error must retry: %v", err)
	}
	if err := h.Handle(c, []byte{0xff, 0xff}); !errors.Is(err, consumer.ErrDrop) {
		t.Fatalf("garbage: %v", err)
	}
}
