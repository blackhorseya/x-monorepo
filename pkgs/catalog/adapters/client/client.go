// Package client is the catalog service seen from another process: a Connect client that
// implements catalogv1connect.CatalogServiceHandler, so the BCs that ask the catalog (ordering's
// pricing, inventory's product check) and apps/rpc's forwarding of catalog.v1 keep one interface
// whether the catalog runs in the same process or behind catalog-rpc.
package client

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"

	catalogv1 "github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1/catalogv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
)

var _ catalogv1connect.CatalogServiceHandler = (*Client)(nil)

// Client asks catalog-rpc over Connect.
type Client struct {
	cc catalogv1connect.CatalogServiceClient
}

// New builds the client for cfg.
func New(cfg config.Catalog, tel *otel.Providers) (*Client, error) {
	return &Client{cc: catalogv1connect.NewCatalogServiceClient(http.DefaultClient, cfg.Upstream)}, nil
}

func (x *Client) UpsertProduct(c context.Context, req *connect.Request[catalogv1.UpsertProductRequest]) (*connect.Response[catalogv1.UpsertProductResponse], error) {
	return x.cc.UpsertProduct(c, req)
}

func (x *Client) GetProduct(c context.Context, req *connect.Request[catalogv1.GetProductRequest]) (*connect.Response[catalogv1.GetProductResponse], error) {
	return x.cc.GetProduct(c, req)
}

func (x *Client) ListProducts(c context.Context, req *connect.Request[catalogv1.ListProductsRequest]) (*connect.Response[catalogv1.ListProductsResponse], error) {
	return x.cc.ListProducts(c, req)
}

func (x *Client) DeactivateProduct(c context.Context, req *connect.Request[catalogv1.DeactivateProductRequest]) (*connect.Response[catalogv1.DeactivateProductResponse], error) {
	return x.cc.DeactivateProduct(c, req)
}

func (x *Client) QuotePrices(c context.Context, req *connect.Request[catalogv1.QuotePricesRequest]) (*connect.Response[catalogv1.QuotePricesResponse], error) {
	return x.cc.QuotePrices(c, req)
}

var _ = errors.New
