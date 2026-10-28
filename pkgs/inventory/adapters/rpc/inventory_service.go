// Package rpc adapts the generated inventory.v1 contract to use cases.
package rpc

import (
	"context"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	inventoryv1 "github.com/blackhorseya/x-monorepo/pkgs/inventory/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/contract/v1/inventoryv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/admin"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

var _ inventoryv1connect.InventoryServiceHandler = (*InventoryService)(nil)

// Use-case aliases keep handler signatures readable.
type (
	SetStockUC             = usecasex.UseCase[dto.SetStockInput, *dto.StockOutput]
	GetStockUC             = usecasex.UseCase[dto.GetStockInput, *dto.StockOutput]
	ListStocksUC           = usecasex.UseCase[dto.ListStocksInput, *dto.ListStocksOutput]
	GetReservationUC       = usecasex.UseCase[dto.GetReservationInput, *dto.ReservationOutput]
	BatchGetReservationsUC = usecasex.UseCase[dto.BatchGetReservationsInput, *dto.BatchGetReservationsOutput]
	ListReservationsUC     = usecasex.UseCase[dto.ListReservationsInput, *dto.ListReservationsOutput]
	ExpireReservationsUC   = usecasex.UseCase[dto.ExpireReservationsInput, *dto.ExpireReservationsOutput]
)

// InventoryService implements the generated InventoryServiceHandler. auth is
// read only by ExpireReservations (server.RequireRole, the admin role): the
// one procedure here that acts on other people's orders.
type InventoryService struct {
	setStock             SetStockUC
	getStock             GetStockUC
	listStocks           ListStocksUC
	getReservation       GetReservationUC
	batchGetReservations BatchGetReservationsUC
	listReservations     ListReservationsUC
	expireReservations   ExpireReservationsUC
	auth                 config.Auth
}

func NewInventoryService(
	setStock SetStockUC,
	getStock GetStockUC,
	listStocks ListStocksUC,
	getReservation GetReservationUC,
	batchGetReservations BatchGetReservationsUC,
	listReservations ListReservationsUC,
	expireReservations ExpireReservationsUC,
	auth config.Auth,
) *InventoryService {
	if auth.AdminRole == "" {
		auth.AdminRole = admin.DefaultAdminRole // config.Load sets the same; this is for hand-built configs (tests)
	}

	return &InventoryService{
		setStock: setStock, getStock: getStock, listStocks: listStocks,
		getReservation: getReservation, batchGetReservations: batchGetReservations, listReservations: listReservations,
		expireReservations: expireReservations, auth: auth,
	}
}

// Route returns the mount path and handler (Connect + gRPC + gRPC-Web on one port).
func (h *InventoryService) Route(opts ...connect.HandlerOption) (string, http.Handler) {
	return inventoryv1connect.NewInventoryServiceHandler(h, opts...)
}

func (h *InventoryService) SetStock(
	c context.Context,
	req *connect.Request[inventoryv1.SetStockRequest],
) (*connect.Response[inventoryv1.SetStockResponse], error) {
	out, err := h.setStock.Execute(c, dto.SetStockInput{SKU: req.Msg.GetSku(), OnHand: req.Msg.GetOnHand()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return connect.NewResponse(&inventoryv1.SetStockResponse{Stock: toProtoStock(out)}), nil
}

func (h *InventoryService) GetStock(
	c context.Context,
	req *connect.Request[inventoryv1.GetStockRequest],
) (*connect.Response[inventoryv1.GetStockResponse], error) {
	out, err := h.getStock.Execute(c, dto.GetStockInput{SKU: req.Msg.GetSku()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return connect.NewResponse(&inventoryv1.GetStockResponse{Stock: toProtoStock(out)}), nil
}

func (h *InventoryService) ListStocks(
	c context.Context,
	req *connect.Request[inventoryv1.ListStocksRequest],
) (*connect.Response[inventoryv1.ListStocksResponse], error) {
	out, err := h.listStocks.Execute(c, dto.ListStocksInput{
		PageSize: req.Msg.GetPageSize(), PageToken: req.Msg.GetPageToken(),
		SKUPrefix: req.Msg.GetSkuPrefix(), OnlyReserved: req.Msg.GetOnlyReserved(),
	})
	if err != nil {
		return nil, server.ToConnectError(err)
	}
	stocks := make([]*inventoryv1.Stock, 0, len(out.Stocks))
	for i := range out.Stocks {
		stocks = append(stocks, toProtoStock(&out.Stocks[i]))
	}

	return connect.NewResponse(&inventoryv1.ListStocksResponse{Stocks: stocks, NextPageToken: out.NextPageToken}), nil
}

func (h *InventoryService) GetReservation(
	c context.Context,
	req *connect.Request[inventoryv1.GetReservationRequest],
) (*connect.Response[inventoryv1.GetReservationResponse], error) {
	out, err := h.getReservation.Execute(c, dto.GetReservationInput{OrderID: req.Msg.GetOrderId()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return connect.NewResponse(&inventoryv1.GetReservationResponse{Reservation: toProtoReservation(out)}), nil
}

func (h *InventoryService) BatchGetReservations(
	c context.Context,
	req *connect.Request[inventoryv1.BatchGetReservationsRequest],
) (*connect.Response[inventoryv1.BatchGetReservationsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func (h *InventoryService) ListReservations(
	c context.Context,
	req *connect.Request[inventoryv1.ListReservationsRequest],
) (*connect.Response[inventoryv1.ListReservationsResponse], error) {
	out, err := h.listReservations.Execute(c, dto.ListReservationsInput{
		Status:    toDTOStatus(req.Msg.GetStatus()),
		PageSize:  req.Msg.GetPageSize(),
		PageToken: req.Msg.GetPageToken(),
	})
	if err != nil {
		return nil, server.ToConnectError(err)
	}
	rs := make([]*inventoryv1.Reservation, 0, len(out.Reservations))
	for i := range out.Reservations {
		rs = append(rs, toProtoReservation(&out.Reservations[i]))
	}

	return connect.NewResponse(&inventoryv1.ListReservationsResponse{Reservations: rs, NextPageToken: out.NextPageToken}), nil
}

// ExpireReservations is the operator's sweep (dry run, small run); the routine
// one is `worker expire`. Admin role when auth is on: it cancels other
// people's orders.
func (h *InventoryService) ExpireReservations(
	c context.Context,
	req *connect.Request[inventoryv1.ExpireReservationsRequest],
) (*connect.Response[inventoryv1.ExpireReservationsResponse], error) {
	if err := server.RequireRole(c, h.auth, h.auth.AdminRole); err != nil {
		return nil, err
	}
	out, err := h.expireReservations.Execute(c, dto.ExpireReservationsInput{
		OlderThan: req.Msg.GetOlderThan().AsDuration(),
		Limit:     int(req.Msg.GetLimit()),
		DryRun:    req.Msg.GetDryRun(),
	})
	if err != nil {
		return nil, server.ToConnectError(err)
	}
	expired := make([]*inventoryv1.ExpiredReservation, 0, len(out.Expired))
	for _, e := range out.Expired {
		expired = append(expired, &inventoryv1.ExpiredReservation{
			OrderId:    e.OrderID,
			ReservedAt: timestamppb.New(e.ReservedAt),
			HeldFor:    durationpb.New(e.HeldFor),
		})
	}

	return connect.NewResponse(&inventoryv1.ExpireReservationsResponse{Expired: expired, DryRun: out.DryRun}), nil
}

// toDTOStatus maps a filter enum to the use case's string filter. UNSPECIFIED
// means "any"; the name is passed through (the inverse of toProtoStatus), so
// a value the server does not know is rejected by the use case instead of
// silently widening the filter.
func toDTOStatus(s inventoryv1.ReservationStatus) string {
	if s == inventoryv1.ReservationStatus_RESERVATION_STATUS_UNSPECIFIED {
		return ""
	}

	return strings.TrimPrefix(s.String(), "RESERVATION_STATUS_")
}

func toProtoStock(s *dto.StockOutput) *inventoryv1.Stock {
	return &inventoryv1.Stock{
		Sku:        s.SKU,
		OnHand:     s.OnHand,
		Reserved:   s.Reserved,
		Available:  s.Available,
		UpdateTime: timestamppb.New(s.UpdatedAt),
	}
}

func toProtoReservation(r *dto.ReservationOutput) *inventoryv1.Reservation {
	lines := make([]*inventoryv1.ReservationLine, 0, len(r.Lines))
	for _, l := range r.Lines {
		lines = append(lines, &inventoryv1.ReservationLine{Sku: l.SKU, Quantity: l.Quantity})
	}

	return &inventoryv1.Reservation{
		OrderId:       r.OrderID,
		Lines:         lines,
		Status:        toProtoStatus(r.Status),
		ShortfallSkus: r.ShortfallSKUs,
		CreateTime:    timestamppb.New(r.CreatedAt),
		UpdateTime:    timestamppb.New(r.UpdatedAt),
	}
}

func toProtoStatus(s string) inventoryv1.ReservationStatus {
	if v, ok := inventoryv1.ReservationStatus_value["RESERVATION_STATUS_"+s]; ok {
		return inventoryv1.ReservationStatus(v)
	}

	return inventoryv1.ReservationStatus_RESERVATION_STATUS_UNSPECIFIED
}
