package main

import (
	"context"
	"net/http"
	"testing"

	"connectrpc.com/connect"

	catalogv1 "github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1/catalogv1connect"
	commonv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/common/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// seedProduct puts a product on sale in the server under test: an order can only be
// placed for a SKU the catalog prices. apiKey is "" when auth is off.
func seedProduct(t *testing.T, baseURL, apiKey, sku string, amount int64, currency string) {
	t.Helper()
	var opts []connect.ClientOption
	if apiKey != "" {
		opts = append(opts, connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(c context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				req.Header().Set(server.APIKeyHeader, apiKey)
				return next(c, req)
			}
		})))
	}
	cl := catalogv1connect.NewCatalogServiceClient(http.DefaultClient, baseURL, opts...)
	if _, err := cl.UpsertProduct(context.Background(), connect.NewRequest(&catalogv1.UpsertProductRequest{
		Sku: sku, Name: "Product " + sku, Price: &commonv1.Money{AmountMinor: amount, Currency: currency},
	})); err != nil {
		t.Fatalf("seed product %s: %v", sku, err)
	}
}
