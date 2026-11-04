package rpc

import (
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	commonv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/common/v1"
)

// pb ↔ dto mapping. Validation is NOT done here; the use case / domain owns it.

func toDTOItems(items []*orderingv1.CreateOrderItem) []dto.OrderLine {
	out := make([]dto.OrderLine, 0, len(items))
	for _, it := range items {
		out = append(out, dto.OrderLine{SKU: it.GetSku(), Quantity: it.GetQuantity()})
	}

	return out
}

func toProtoOrder(o *dto.OrderOutput) *orderingv1.Order {
	items := make([]*orderingv1.LineItem, 0, len(o.Items))
	for _, it := range o.Items {
		items = append(items, &orderingv1.LineItem{
			Sku:       it.SKU,
			Quantity:  it.Quantity,
			UnitPrice: toProtoMoney(it.UnitPrice),
		})
	}

	return &orderingv1.Order{
		Id:         o.ID,
		CustomerId: o.CustomerID,
		Items:      items,
		Total:      toProtoMoney(o.Total),
		Status:     toProtoStatus(o.Status),
		CreateTime: timestamppb.New(o.CreatedAt),
		UpdateTime: timestamppb.New(o.UpdatedAt),

		PaymentRequired: o.PaymentRequired,
	}
}

func toProtoMoney(m dto.Money) *commonv1.Money {
	return &commonv1.Money{AmountMinor: m.AmountMinor, Currency: m.Currency}
}

func toProtoStatus(s string) orderingv1.OrderStatus {
	switch s {
	case "PENDING":
		return orderingv1.OrderStatus_ORDER_STATUS_PENDING
	case "CANCELLED":
		return orderingv1.OrderStatus_ORDER_STATUS_CANCELLED
	case "CONFIRMED":
		return orderingv1.OrderStatus_ORDER_STATUS_CONFIRMED
	case "COMPLETED":
		return orderingv1.OrderStatus_ORDER_STATUS_COMPLETED
	case "PAID":
		return orderingv1.OrderStatus_ORDER_STATUS_PAID
	default:
		return orderingv1.OrderStatus_ORDER_STATUS_UNSPECIFIED
	}
}

// toDTOStatus maps a filter enum to the use case's string filter. UNSPECIFIED
// means "any"; anything the server does not know is passed through by name so
// the use case rejects it instead of silently widening the filter.
func toDTOStatus(s orderingv1.OrderStatus) string {
	switch s {
	case orderingv1.OrderStatus_ORDER_STATUS_UNSPECIFIED:
		return ""
	case orderingv1.OrderStatus_ORDER_STATUS_PENDING:
		return "PENDING"
	case orderingv1.OrderStatus_ORDER_STATUS_CANCELLED:
		return "CANCELLED"
	case orderingv1.OrderStatus_ORDER_STATUS_CONFIRMED:
		return "CONFIRMED"
	case orderingv1.OrderStatus_ORDER_STATUS_COMPLETED:
		return "COMPLETED"
	case orderingv1.OrderStatus_ORDER_STATUS_PAID:
		return "PAID"
	default:
		return s.String()
	}
}

func toProtoHistoryEntry(e *dto.HistoryEntryOutput) *orderingv1.OrderHistoryEntry {
	return &orderingv1.OrderHistoryEntry{
		Seq:          e.Seq,
		FromStatus:   toProtoStatus(e.From),
		ToStatus:     toProtoStatus(e.To),
		OccurredTime: timestamppb.New(e.OccurredAt),
		ActorKind:    toProtoActorKind(e.ActorKind),
		ActorSubject: e.ActorSubject,
		Cause:        toProtoHistoryCause(e.Cause),
	}
}

func toProtoActorKind(s string) orderingv1.OrderHistoryActorKind {
	if v, ok := orderingv1.OrderHistoryActorKind_value["ORDER_HISTORY_ACTOR_KIND_"+s]; ok {
		return orderingv1.OrderHistoryActorKind(v)
	}

	return orderingv1.OrderHistoryActorKind_ORDER_HISTORY_ACTOR_KIND_UNSPECIFIED
}

// toProtoHistoryCause: a cause name this wire version does not have is UNSPECIFIED, never an error.
func toProtoHistoryCause(s string) orderingv1.OrderHistoryCause {
	if v, ok := orderingv1.OrderHistoryCause_value["ORDER_HISTORY_CAUSE_"+s]; ok {
		return orderingv1.OrderHistoryCause(v)
	}

	return orderingv1.OrderHistoryCause_ORDER_HISTORY_CAUSE_UNSPECIFIED
}
