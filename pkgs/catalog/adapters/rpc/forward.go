package rpc

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"

	catalogv1 "github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1/catalogv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/admin"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

var _ catalogv1connect.CatalogServiceHandler = (*Forwarder)(nil)

// Forwarder serves catalog.v1 on apps/rpc's port for the catalog that lives behind catalog-rpc: the
// URL, the auth chain, the rate limit and the OpenAPI document are rpc's, unchanged. What changes is
// who answers. Two rules:
//
//   - The role is checked HERE, on the caller's own principal, before anything is forwarded. The
//     upstream sees only rpc's service key (an admin one, so that rpc can write at all), so a write
//     that skipped this check would be an admin write for anyone: UpsertProduct and DeactivateProduct
//     take auth.admin_role like they do inside catalog-rpc; the reads stay open to any caller the
//     auth interceptor lets in.
//   - The answer is the catalog's and nothing of the hop. A business error keeps its code, message,
//     details and x-error-code; the upstream's transport headers (Content-Type, traceparent, ...) do
//     not follow it, nor do they follow a success: the response is rebuilt from the message.
type Forwarder struct {
	upstream catalogv1connect.CatalogServiceHandler
	auth     config.Auth
}

func NewForwarder(upstream catalogv1connect.CatalogServiceHandler, auth config.Auth) *Forwarder {
	if auth.AdminRole == "" {
		auth.AdminRole = admin.DefaultAdminRole // config.Load sets the same; this is for hand-built configs (tests)
	}

	return &Forwarder{upstream: upstream, auth: auth}
}

// Route returns the mount path and handler (Connect + gRPC + gRPC-Web on one port).
func (h *Forwarder) Route(opts ...connect.HandlerOption) (string, http.Handler) {
	return catalogv1connect.NewCatalogServiceHandler(h, opts...)
}

// answer rebuilds the upstream's response from its message and cleans its error.
func answer[T any](res *connect.Response[T], err error) (*connect.Response[T], error) {
	if err != nil {
		return nil, clean(err)
	}

	return connect.NewResponse(res.Msg), nil
}

// clean keeps what the caller may see of an upstream error: code, message, details and x-error-code.
func clean(err error) error {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	out := connect.NewError(ce.Code(), errors.New(ce.Message()))
	for _, d := range ce.Details() {
		out.AddDetail(d)
	}
	if v := ce.Meta().Get("x-error-code"); v != "" {
		out.Meta().Set("x-error-code", v)
	}

	return out
}

func (h *Forwarder) UpsertProduct(c context.Context, req *connect.Request[catalogv1.UpsertProductRequest]) (*connect.Response[catalogv1.UpsertProductResponse], error) {
	if err := server.RequireRole(c, h.auth, h.auth.AdminRole); err != nil {
		return nil, err
	}

	return answer(h.upstream.UpsertProduct(c, req))
}

func (h *Forwarder) GetProduct(c context.Context, req *connect.Request[catalogv1.GetProductRequest]) (*connect.Response[catalogv1.GetProductResponse], error) {
	return answer(h.upstream.GetProduct(c, req))
}

func (h *Forwarder) ListProducts(c context.Context, req *connect.Request[catalogv1.ListProductsRequest]) (*connect.Response[catalogv1.ListProductsResponse], error) {
	return answer(h.upstream.ListProducts(c, req))
}

func (h *Forwarder) DeactivateProduct(c context.Context, req *connect.Request[catalogv1.DeactivateProductRequest]) (*connect.Response[catalogv1.DeactivateProductResponse], error) {
	if err := server.RequireRole(c, h.auth, h.auth.AdminRole); err != nil {
		return nil, err
	}

	return answer(h.upstream.DeactivateProduct(c, req))
}

func (h *Forwarder) QuotePrices(c context.Context, req *connect.Request[catalogv1.QuotePricesRequest]) (*connect.Response[catalogv1.QuotePricesResponse], error) {
	return answer(h.upstream.QuotePrices(c, req))
}
