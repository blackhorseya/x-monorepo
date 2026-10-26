package rpc

import (
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	commonv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/common/v1"
)

// pb ↔ dto mapping. Validation is NOT done here; the use case / domain owns it.

func toDTOItems(items []*orderingv1.LineItem) []dto.LineItem {
	out := make([]dto.LineItem, 0, len(items))
	for _, it := range items {
		out = append(out, dto.LineItem{
			SKU:      it.GetSku(),
			Quantity: it.GetQuantity(),
			UnitPrice: dto.Money{
				AmountMinor: it.GetUnitPrice().GetAmountMinor(),
				Currency:    it.GetUnitPrice().GetCurrency(),
			},
		})
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
	default:
		return s.String()
	}
}
