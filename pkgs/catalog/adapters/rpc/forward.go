package rpc

import (
	"context"
	"net/http"

	"connectrpc.com/connect"

	catalogv1 "github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1/catalogv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

var _ catalogv1connect.CatalogServiceHandler = (*Forwarder)(nil)

// Forwarder serves catalog.v1 on apps/rpc's port for the catalog that lives behind catalog-rpc.
type Forwarder struct {
	upstream catalogv1connect.CatalogServiceHandler
	auth     config.Auth
}

func NewForwarder(upstream catalogv1connect.CatalogServiceHandler, auth config.Auth) *Forwarder {
	return &Forwarder{upstream: upstream, auth: auth}
}

// Route returns the mount path and handler (Connect + gRPC + gRPC-Web on one port).
func (h *Forwarder) Route(opts ...connect.HandlerOption) (string, http.Handler) {
	return catalogv1connect.NewCatalogServiceHandler(h, opts...)
}

func (h *Forwarder) UpsertProduct(c context.Context, req *connect.Request[catalogv1.UpsertProductRequest]) (*connect.Response[catalogv1.UpsertProductResponse], error) {
	return h.upstream.UpsertProduct(c, req)
}

func (h *Forwarder) GetProduct(c context.Context, req *connect.Request[catalogv1.GetProductRequest]) (*connect.Response[catalogv1.GetProductResponse], error) {
	return h.upstream.GetProduct(c, req)
}

func (h *Forwarder) ListProducts(c context.Context, req *connect.Request[catalogv1.ListProductsRequest]) (*connect.Response[catalogv1.ListProductsResponse], error) {
	return h.upstream.ListProducts(c, req)
}

func (h *Forwarder) DeactivateProduct(c context.Context, req *connect.Request[catalogv1.DeactivateProductRequest]) (*connect.Response[catalogv1.DeactivateProductResponse], error) {
	return h.upstream.DeactivateProduct(c, req)
}

func (h *Forwarder) QuotePrices(c context.Context, req *connect.Request[catalogv1.QuotePricesRequest]) (*connect.Response[catalogv1.QuotePricesResponse], error) {
	return h.upstream.QuotePrices(c, req)
}
