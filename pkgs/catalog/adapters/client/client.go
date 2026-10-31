// Package client is the catalog service seen from another process: a Connect client that
// implements catalogv1connect.CatalogServiceHandler, so the BCs that ask the catalog (ordering's
// pricing, inventory's product check) and apps/rpc's forwarding of catalog.v1 keep one interface
// whether the catalog runs in the same process or behind catalog-rpc.
//
// Two rules live here and nowhere else:
//
//   - Credentials. The only credential that reaches the catalog is the service key from
//     config.Catalog. Each call builds a fresh request from the message alone, so nothing the
//     caller presented (a user's X-API-Key, a bearer token, a cookie) is forwarded.
//   - Failures. A business answer of the catalog (NOT_FOUND, INVALID_ARGUMENT, FAILED_PRECONDITION,
//     ALREADY_EXISTS ...) passes through unchanged, x-error-code included. Everything else, the
//     catalog not answering for any reason, is one error: UNAVAILABLE with x-error-code
//     catalog.unavailable and a fixed message. The cause goes to the log, not to the caller; a
//     service key the catalog rejects looks the same to the caller as an outage (it is the
//     deployment's mistake, not theirs) and is logged as such.
package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"

	catalogv1 "github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1/catalogv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
)

var _ catalogv1connect.CatalogServiceHandler = (*Client)(nil)

// UnavailableCode is the x-error-code of the catalog not answering.
const UnavailableCode = "catalog.unavailable"

// APIKeyHeader carries the service key (the header the catalog's auth interceptor reads).
const APIKeyHeader = "X-API-Key"

// Client asks catalog-rpc over Connect.
type Client struct {
	cc      catalogv1connect.CatalogServiceClient
	timeout time.Duration
}

// New builds the client for cfg: an http(s) upstream, a service key and a positive per-call budget
// are required. No error carries the key.
func New(cfg config.Catalog, tel *otel.Providers) (*Client, error) {
	u, err := url.Parse(cfg.Upstream)
	if cfg.Upstream == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("catalog client: catalog.upstream must be an http(s) URL")
	}
	if cfg.APIKey == "" {
		return nil, errors.New("catalog client: catalog.api_key (APP_CATALOG_API_KEY) is empty")
	}
	if cfg.Timeout <= 0 {
		return nil, fmt.Errorf("catalog client: catalog.timeout %s must be > 0", cfg.Timeout)
	}
	tracing, err := otelconnect.NewInterceptor(
		otelconnect.WithTracerProvider(tel.Tracer),
		otelconnect.WithMeterProvider(tel.Meter),
		otelconnect.WithPropagator(tel.Propagator),
	)
	if err != nil {
		return nil, fmt.Errorf("catalog client: otel interceptor: %w", err)
	}
	key := cfg.APIKey
	withKey := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(c context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set(APIKeyHeader, key)

			return next(c, req)
		}
	})

	return &Client{
		cc: catalogv1connect.NewCatalogServiceClient(&http.Client{}, cfg.Upstream,
			connect.WithInterceptors(tracing, withKey)),
		timeout: cfg.Timeout,
	}, nil
}

func call[Req, Res any](
	x *Client, c context.Context, op string, in *Req,
	do func(context.Context, *connect.Request[Req]) (*connect.Response[Res], error),
) (*connect.Response[Res], error) {
	cc, cancel := context.WithTimeout(c, x.timeout)
	defer cancel()

	out, err := do(cc, connect.NewRequest(in)) // a fresh request: the caller's headers stay behind
	if err == nil {
		return out, nil
	}

	return nil, classify(c, op, err)
}

// classify is the failure matrix. c is the caller's context (not the per-call one).
func classify(c context.Context, op string, err error) error {
	if errors.Is(c.Err(), context.Canceled) {
		return connect.NewError(connect.CodeCanceled, errors.New("request canceled"))
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		switch ce.Code() {
		case connect.CodeNotFound, connect.CodeInvalidArgument, connect.CodeFailedPrecondition,
			connect.CodeAlreadyExists, connect.CodeOutOfRange:
			return err // the catalog's own answer, code and x-error-code intact
		case connect.CodeUnauthenticated, connect.CodePermissionDenied:
			contextx.FromContext(c).Error("catalog service credential rejected: check APP_CATALOG_API_KEY against the catalog's auth.api_keys",
				"op", op, "code", ce.Code().String())

			return unavailable()
		}
		contextx.FromContext(c).Warn("catalog call failed", "op", op, "code", ce.Code().String(), "error", ce.Message())

		return unavailable()
	}
	contextx.FromContext(c).Warn("catalog call failed", "op", op, "error", err.Error())

	return unavailable()
}

func unavailable() error {
	ce := connect.NewError(connect.CodeUnavailable, errors.New("the catalog could not be reached, retry the request"))
	ce.Meta().Set("x-error-code", UnavailableCode)

	return ce
}

func (x *Client) UpsertProduct(c context.Context, req *connect.Request[catalogv1.UpsertProductRequest]) (*connect.Response[catalogv1.UpsertProductResponse], error) {
	return call(x, c, "UpsertProduct", req.Msg, x.cc.UpsertProduct)
}

func (x *Client) GetProduct(c context.Context, req *connect.Request[catalogv1.GetProductRequest]) (*connect.Response[catalogv1.GetProductResponse], error) {
	return call(x, c, "GetProduct", req.Msg, x.cc.GetProduct)
}

func (x *Client) ListProducts(c context.Context, req *connect.Request[catalogv1.ListProductsRequest]) (*connect.Response[catalogv1.ListProductsResponse], error) {
	return call(x, c, "ListProducts", req.Msg, x.cc.ListProducts)
}

func (x *Client) DeactivateProduct(c context.Context, req *connect.Request[catalogv1.DeactivateProductRequest]) (*connect.Response[catalogv1.DeactivateProductResponse], error) {
	return call(x, c, "DeactivateProduct", req.Msg, x.cc.DeactivateProduct)
}

func (x *Client) QuotePrices(c context.Context, req *connect.Request[catalogv1.QuotePricesRequest]) (*connect.Response[catalogv1.QuotePricesResponse], error) {
	return call(x, c, "QuotePrices", req.Msg, x.cc.QuotePrices)
}
