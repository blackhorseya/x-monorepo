package catalog_test

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	catalogv1 "github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1/catalogv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/adapters/catalog"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/port"
)

// fakeCatalog is the catalog's contract, as the adapter sees it: only GetProduct is used.
type fakeCatalog struct {
	catalogv1connect.UnimplementedCatalogServiceHandler
	got string
	res *catalogv1.GetProductResponse
	err error
}

func (f *fakeCatalog) GetProduct(_ context.Context, req *connect.Request[catalogv1.GetProductRequest]) (*connect.Response[catalogv1.GetProductResponse], error) {
	f.got = req.Msg.GetSku()
	if f.err != nil {
		return nil, f.err
	}

	return connect.NewResponse(f.res), nil
}

func TestCatalog_AProductIsThere_OnSaleOrNot(t *testing.T) {
	for name, active := range map[string]bool{"on sale": true, "deactivated": false} {
		t.Run(name, func(t *testing.T) {
			cat := &fakeCatalog{res: &catalogv1.GetProductResponse{Product: &catalogv1.Product{Sku: "A", Active: active}}}
			ok, err := catalog.NewCatalog(cat).HasProduct(context.Background(), "A")
			if err != nil || !ok || cat.got != "A" {
				t.Fatalf("HasProduct = %v, %v (asked %q), want true", ok, err, cat.got)
			}
		})
	}
}

func TestCatalog_NotFoundIsNoSuchProduct(t *testing.T) {
	nf := connect.NewError(connect.CodeNotFound, errors.New("product not found"))
	ok, err := catalog.NewCatalog(&fakeCatalog{err: nf}).HasProduct(context.Background(), "A")
	if err != nil || ok {
		t.Fatalf("HasProduct = %v, %v, want false with no error", ok, err)
	}
}

// Anything but NOT_FOUND is the catalog failing to answer, never a guess either way: the
// error says so (port.ErrCatalogUnavailable) and keeps the cause.
func TestCatalog_EveryOtherFailureIsUnavailable(t *testing.T) {
	for _, code := range []connect.Code{
		connect.CodeInternal, connect.CodeUnavailable, connect.CodeInvalidArgument,
		connect.CodeDeadlineExceeded, connect.CodePermissionDenied, connect.CodeUnknown,
	} {
		t.Run(code.String(), func(t *testing.T) {
			boom := connect.NewError(code, errors.New("catalog said no"))
			ok, err := catalog.NewCatalog(&fakeCatalog{err: boom}).HasProduct(context.Background(), "A")
			if ok || !errors.Is(err, port.ErrCatalogUnavailable) || !errors.Is(err, boom) {
				t.Fatalf("HasProduct = %v, %v; want false, an error wrapping ErrCatalogUnavailable and the cause", ok, err)
			}
		})
	}

	ok, err := catalog.NewCatalog(&fakeCatalog{err: context.DeadlineExceeded}).HasProduct(context.Background(), "A")
	if ok || !errors.Is(err, port.ErrCatalogUnavailable) {
		t.Fatalf("a plain error: %v, %v", ok, err)
	}
}
