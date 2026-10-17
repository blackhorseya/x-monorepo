// Package rpc adapts the generated ordering.v1 contract to use cases (go-kit's
// "transport"). It is one of only two packages in this BC that import contract/.
package rpc

import (
	"context"
	"net/http"

	"connectrpc.com/connect"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1/orderingv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

var _ orderingv1connect.OrderServiceHandler = (*OrderService)(nil)

// Use-case aliases keep handler signatures readable.
type (
	CreateOrderUC = usecasex.UseCase[dto.CreateOrderInput, *dto.OrderOutput]
	GetOrderUC    = usecasex.UseCase[dto.GetOrderInput, *dto.OrderOutput]
	CancelOrderUC = usecasex.UseCase[dto.CancelOrderInput, *dto.OrderOutput]
	ListOrdersUC  = usecasex.UseCase[dto.ListOrdersInput, *dto.ListOrdersOutput]
)

// OrderService implements the generated OrderServiceHandler.
type OrderService struct {
	createOrder CreateOrderUC
	getOrder    GetOrderUC
	cancelOrder CancelOrderUC
	listOrders  ListOrdersUC
}

func NewOrderService(createOrder CreateOrderUC, getOrder GetOrderUC, cancelOrder CancelOrderUC, listOrders ListOrdersUC) *OrderService {
	return &OrderService{createOrder: createOrder, getOrder: getOrder, cancelOrder: cancelOrder, listOrders: listOrders}
}

// Route returns the mount path and handler (Connect + gRPC + gRPC-Web on one port).
func (h *OrderService) Route(opts ...connect.HandlerOption) (string, http.Handler) {
	return orderingv1connect.NewOrderServiceHandler(h, opts...)
}

func (h *OrderService) CreateOrder(
	c context.Context,
	req *connect.Request[orderingv1.CreateOrderRequest],
) (*connect.Response[orderingv1.CreateOrderResponse], error) {
	out, err := h.createOrder.Execute(c, dto.CreateOrderInput{
		CustomerID: req.Msg.GetCustomerId(),
		Items:      toDTOItems(req.Msg.GetItems()),
	})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return connect.NewResponse(&orderingv1.CreateOrderResponse{Order: toProtoOrder(out)}), nil
}

func (h *OrderService) GetOrder(
	c context.Context,
	req *connect.Request[orderingv1.GetOrderRequest],
) (*connect.Response[orderingv1.GetOrderResponse], error) {
	out, err := h.getOrder.Execute(c, dto.GetOrderInput{OrderID: req.Msg.GetId()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return connect.NewResponse(&orderingv1.GetOrderResponse{Order: toProtoOrder(out)}), nil
}

func (h *OrderService) CancelOrder(
	c context.Context,
	req *connect.Request[orderingv1.CancelOrderRequest],
) (*connect.Response[orderingv1.CancelOrderResponse], error) {
	out, err := h.cancelOrder.Execute(c, dto.CancelOrderInput{OrderID: req.Msg.GetId()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return connect.NewResponse(&orderingv1.CancelOrderResponse{Order: toProtoOrder(out)}), nil
}

func (h *OrderService) ListOrders(
	c context.Context,
	req *connect.Request[orderingv1.ListOrdersRequest],
) (*connect.Response[orderingv1.ListOrdersResponse], error) {
	out, err := h.listOrders.Execute(c, dto.ListOrdersInput{
		CustomerID: req.Msg.GetCustomerId(),
		Status:     toDTOStatus(req.Msg.GetStatus()),
		PageSize:   req.Msg.GetPageSize(),
		PageToken:  req.Msg.GetPageToken(),
	})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	orders := make([]*orderingv1.Order, 0, len(out.Orders))
	for i := range out.Orders {
		orders = append(orders, toProtoOrder(&out.Orders[i]))
	}

	return connect.NewResponse(&orderingv1.ListOrdersResponse{Orders: orders, NextPageToken: out.NextPageToken}), nil
}
