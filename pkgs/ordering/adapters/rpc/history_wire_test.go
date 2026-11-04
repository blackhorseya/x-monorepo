package rpc_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/rpc"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
)

type historyReturns struct{ out *dto.GetOrderHistoryOutput }

func (f historyReturns) Execute(context.Context, dto.GetOrderHistoryInput) (*dto.GetOrderHistoryOutput, error) {
	return f.out, nil
}

// The wire carries every column of an entry, and a name this wire version has no enum value for (a
// cause a newer ordering wrote, an actor it added) is UNSPECIFIED rather than an error: the RPC keeps
// answering, and a client treats UNSPECIFIED as "not classified".
func TestWire_HistoryEntryAndUnknownNames(t *testing.T) {
	at := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	svc := rpc.NewOrderService(nil, nil, nil, nil, nil, historyReturns{&dto.GetOrderHistoryOutput{Entries: []dto.HistoryEntryOutput{
		{Seq: 1, To: "PENDING", OccurredAt: at, ActorKind: "CUSTOMER", ActorSubject: "acc-1", Cause: "RPC_REQUEST"},
		{Seq: 2, From: "PENDING", To: "CANCELLED", OccurredAt: at, ActorKind: "SYSTEM", Cause: "STOCK_SHORTAGE"},
		{Seq: 3, From: "CANCELLED", To: "CANCELLED", OccurredAt: at, ActorKind: "A_NEW_ACTOR", Cause: "A_CAUSE_FROM_THE_FUTURE"},
	}}})
	res, err := svc.GetOrderHistory(context.Background(), connect.NewRequest(&orderingv1.GetOrderHistoryRequest{Id: "o-1"}))
	if err != nil {
		t.Fatal(err)
	}
	got := res.Msg.GetEntries()
	if len(got) != 3 {
		t.Fatalf("entries = %v", got)
	}

	first := got[0]
	if first.GetSeq() != 1 || first.GetFromStatus() != orderingv1.OrderStatus_ORDER_STATUS_UNSPECIFIED || first.GetToStatus() != orderingv1.OrderStatus_ORDER_STATUS_PENDING ||
		!first.GetOccurredTime().AsTime().Equal(at) || first.GetActorKind() != orderingv1.OrderHistoryActorKind_ORDER_HISTORY_ACTOR_KIND_CUSTOMER ||
		first.GetActorSubject() != "acc-1" || first.GetCause() != orderingv1.OrderHistoryCause_ORDER_HISTORY_CAUSE_RPC_REQUEST {
		t.Errorf("first entry = %v", first)
	}
	if got[1].GetCause() != orderingv1.OrderHistoryCause_ORDER_HISTORY_CAUSE_STOCK_SHORTAGE || got[1].GetActorKind() != orderingv1.OrderHistoryActorKind_ORDER_HISTORY_ACTOR_KIND_SYSTEM {
		t.Errorf("second entry = %v", got[1])
	}
	if got[2].GetCause() != orderingv1.OrderHistoryCause_ORDER_HISTORY_CAUSE_UNSPECIFIED || got[2].GetActorKind() != orderingv1.OrderHistoryActorKind_ORDER_HISTORY_ACTOR_KIND_UNSPECIFIED {
		t.Errorf("unknown names = %v / %v, want UNSPECIFIED", got[2].GetActorKind(), got[2].GetCause())
	}
}
