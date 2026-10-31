// Package rpc adapts the generated catalog.v1 contract to use cases.
package rpc

import (
	"context"
	"net/http"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/blackhorseya/x-monorepo/pkgs/catalog/application/dto"
	catalogv1 "github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1/catalogv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/admin"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	commonv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/common/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

var _ catalogv1connect.CatalogServiceHandler = (*CatalogService)(nil)

// Use-case aliases keep handler signatures readable.
type (
	UpsertProductUC     = usecasex.UseCase[dto.UpsertProductInput, *dto.ProductOutput]
	GetProductUC        = usecasex.UseCase[dto.GetProductInput, *dto.ProductOutput]
	ListProductsUC      = usecasex.UseCase[dto.ListProductsInput, *dto.ListProductsOutput]
	DeactivateProductUC = usecasex.UseCase[dto.DeactivateProductInput, *dto.ProductOutput]
	QuotePricesUC       = usecasex.UseCase[dto.QuotePricesInput, *dto.QuotePricesOutput]
)

// CatalogService implements the generated CatalogServiceHandler. auth is read
// by the two writes, UpsertProduct and DeactivateProduct (server.RequireRole,
// the admin role): they decide what customers are charged. The reads stay open
// to any caller the auth interceptor lets in.
type CatalogService struct {
	upsertProduct     UpsertProductUC
	getProduct        GetProductUC
	listProducts      ListProductsUC
	deactivateProduct DeactivateProductUC
	quotePrices       QuotePricesUC
	auth              config.Auth
}

func NewCatalogService(
	upsertProduct UpsertProductUC,
	getProduct GetProductUC,
	listProducts ListProductsUC,
	deactivateProduct DeactivateProductUC,
	quotePrices QuotePricesUC,
	auth config.Auth,
) *CatalogService {
	if auth.AdminRole == "" {
		auth.AdminRole = admin.DefaultAdminRole // config.Load sets the same; this is for hand-built configs (tests)
	}

	return &CatalogService{
		upsertProduct: upsertProduct, getProduct: getProduct, listProducts: listProducts,
		deactivateProduct: deactivateProduct, quotePrices: quotePrices, auth: auth,
	}
}

// Route returns the mount path and handler (Connect + gRPC + gRPC-Web on one port).
func (h *CatalogService) Route(opts ...connect.HandlerOption) (string, http.Handler) {
	return catalogv1connect.NewCatalogServiceHandler(h, opts...)
}

// UpsertProduct creates a product or replaces its name and price. Admin role when auth is on.
func (h *CatalogService) UpsertProduct(
	c context.Context,
	req *connect.Request[catalogv1.UpsertProductRequest],
) (*connect.Response[catalogv1.UpsertProductResponse], error) {
	if err := server.RequireRole(c, h.auth, h.auth.AdminRole); err != nil {
		return nil, err
	}
	out, err := h.upsertProduct.Execute(c, dto.UpsertProductInput{
		SKU:      req.Msg.GetSku(),
		Name:     req.Msg.GetName(),
		Amount:   req.Msg.GetPrice().GetAmountMinor(),
		Currency: req.Msg.GetPrice().GetCurrency(),
	})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return connect.NewResponse(&catalogv1.UpsertProductResponse{Product: toProtoProduct(out)}), nil
}

func (h *CatalogService) GetProduct(
	c context.Context,
	req *connect.Request[catalogv1.GetProductRequest],
) (*connect.Response[catalogv1.GetProductResponse], error) {
	out, err := h.getProduct.Execute(c, dto.GetProductInput{SKU: req.Msg.GetSku()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return connect.NewResponse(&catalogv1.GetProductResponse{Product: toProtoProduct(out)}), nil
}

func (h *CatalogService) ListProducts(
	c context.Context,
	req *connect.Request[catalogv1.ListProductsRequest],
) (*connect.Response[catalogv1.ListProductsResponse], error) {
	out, err := h.listProducts.Execute(c, dto.ListProductsInput{
		PageSize: req.Msg.GetPageSize(), PageToken: req.Msg.GetPageToken(), SKUPrefix: req.Msg.GetSkuPrefix(),
	})
	if err != nil {
		return nil, server.ToConnectError(err)
	}
	res := &catalogv1.ListProductsResponse{NextPageToken: out.NextPageToken}
	for i := range out.Products {
		res.Products = append(res.Products, toProtoProduct(&out.Products[i]))
	}

	return connect.NewResponse(res), nil
}

// DeactivateProduct takes a product off sale. Admin role when auth is on.
func (h *CatalogService) DeactivateProduct(
	c context.Context,
	req *connect.Request[catalogv1.DeactivateProductRequest],
) (*connect.Response[catalogv1.DeactivateProductResponse], error) {
	if err := server.RequireRole(c, h.auth, h.auth.AdminRole); err != nil {
		return nil, err
	}
	out, err := h.deactivateProduct.Execute(c, dto.DeactivateProductInput{SKU: req.Msg.GetSku()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return connect.NewResponse(&catalogv1.DeactivateProductResponse{Product: toProtoProduct(out)}), nil
}

// QuotePrices is a read: any caller the auth interceptor lets in may ask what things cost.
// Ordering asks it to price an order: through apps/rpc's Connect client to catalog-rpc, which presents
// rpc's service key (a read, so any verified credential may).
func (h *CatalogService) QuotePrices(
	c context.Context,
	req *connect.Request[catalogv1.QuotePricesRequest],
) (*connect.Response[catalogv1.QuotePricesResponse], error) {
	out, err := h.quotePrices.Execute(c, dto.QuotePricesInput{SKUs: req.Msg.GetSkus()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}
	res := &catalogv1.QuotePricesResponse{
		Prices:   make(map[string]*commonv1.Money, len(out.Prices)),
		Missing:  out.Missing,
		Inactive: out.Inactive,
	}
	for sku, p := range out.Prices {
		res.Prices[sku] = &commonv1.Money{AmountMinor: p.Amount, Currency: p.Currency}
	}

	return connect.NewResponse(res), nil
}

func toProtoProduct(p *dto.ProductOutput) *catalogv1.Product {
	return &catalogv1.Product{
		Sku:        p.SKU,
		Name:       p.Name,
		Price:      &commonv1.Money{AmountMinor: p.Price.Amount, Currency: p.Price.Currency},
		Active:     p.Active,
		UpdateTime: timestamppb.New(p.UpdateTime),
	}
}
