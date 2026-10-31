package rpc_test

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	"github.com/blackhorseya/x-monorepo/pkgs/catalog/adapters/rpc"
	catalogv1 "github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1/catalogv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	commonv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/common/v1"
)

// upstream is what the forwarder forwards to: it counts calls and answers per test.
type upstream struct {
	catalogv1connect.UnimplementedCatalogServiceHandler
	calls int
	err   error
}

func (u *upstream) UpsertProduct(context.Context, *connect.Request[catalogv1.UpsertProductRequest]) (*connect.Response[catalogv1.UpsertProductResponse], error) {
	u.calls++
	if u.err != nil {
		return nil, u.err
	}
	res := connect.NewResponse(&catalogv1.UpsertProductResponse{Product: &catalogv1.Product{Sku: "A"}})
	res.Header().Set("Content-Type", "application/x-from-upstream")
	res.Header().Set("Traceparent", "00-upstream-upstream-01")

	return res, nil
}

func (u *upstream) DeactivateProduct(context.Context, *connect.Request[catalogv1.DeactivateProductRequest]) (*connect.Response[catalogv1.DeactivateProductResponse], error) {
	u.calls++

	return connect.NewResponse(&catalogv1.DeactivateProductResponse{}), u.err
}

func (u *upstream) GetProduct(context.Context, *connect.Request[catalogv1.GetProductRequest]) (*connect.Response[catalogv1.GetProductResponse], error) {
	u.calls++
	if u.err != nil {
		return nil, u.err
	}

	return connect.NewResponse(&catalogv1.GetProductResponse{Product: &catalogv1.Product{Sku: "A"}}), nil
}

func upsertReq() *connect.Request[catalogv1.UpsertProductRequest] {
	return connect.NewRequest(&catalogv1.UpsertProductRequest{Sku: "A", Name: "A", Price: &commonv1.Money{AmountMinor: 1, Currency: "USD"}})
}

func as(roles ...string) context.Context {
	return contextx.WithPrincipal(context.Background(), contextx.Principal{Subject: "u", Roles: roles, Method: "api_key"})
}

var required = config.Auth{Mode: config.AuthRequired, AdminRole: "admin"}

// The writes decide what customers are charged. Forwarding them with the SERVICE key must not turn
// every caller into an admin: the role is checked here, at rpc, on the user's principal, before
// the upstream is touched.
func TestForwarder_WritesNeedTheAdminRoleAtRPC(t *testing.T) {
	for name, call := range map[string]func(*rpc.Forwarder, context.Context) error{
		"UpsertProduct": func(f *rpc.Forwarder, c context.Context) error {
			_, err := f.UpsertProduct(c, upsertReq())
			return err
		},
		"DeactivateProduct": func(f *rpc.Forwarder, c context.Context) error {
			_, err := f.DeactivateProduct(c, connect.NewRequest(&catalogv1.DeactivateProductRequest{Sku: "A"}))
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			up := &upstream{}
			f := rpc.NewForwarder(up, required)

			// role-less credential: PERMISSION_DENIED, never forwarded
			err := call(f, as())
			if connect.CodeOf(err) != connect.CodePermissionDenied || up.calls != 0 {
				t.Fatalf("role-less: err = %v, upstream calls = %d; want PERMISSION_DENIED and no call", err, up.calls)
			}
			// a role that is not auth.admin_role
			if err := call(f, as("operator")); connect.CodeOf(err) != connect.CodePermissionDenied || up.calls != 0 {
				t.Fatalf("other role: err = %v, calls = %d", err, up.calls)
			}
			// no credential at all
			if err := call(f, context.Background()); connect.CodeOf(err) != connect.CodeUnauthenticated || up.calls != 0 {
				t.Fatalf("anonymous: err = %v, calls = %d", err, up.calls)
			}
			// the admin role goes through
			if err := call(f, as("admin")); err != nil || up.calls != 1 {
				t.Fatalf("admin: err = %v, calls = %d, want one forwarded call", err, up.calls)
			}
			// auth off is the deployment's choice: open, as before
			up2 := &upstream{}
			if err := call(rpc.NewForwarder(up2, config.Auth{Mode: config.AuthOff}), context.Background()); err != nil || up2.calls != 1 {
				t.Fatalf("auth off: err = %v, calls = %d", err, up2.calls)
			}
		})
	}

	t.Run("reads stay open to any verified credential", func(t *testing.T) {
		up := &upstream{}
		f := rpc.NewForwarder(up, required)
		if _, err := f.GetProduct(as(), connect.NewRequest(&catalogv1.GetProductRequest{Sku: "A"})); err != nil || up.calls != 1 {
			t.Fatalf("role-less read: err = %v, calls = %d", err, up.calls)
		}
	})
}

// What the caller gets back is the catalog's answer and nothing of the hop: the business error with
// its code and x-error-code and details, never the upstream's transport headers.
func TestForwarder_AnswersAreTheCatalogsAndNothingMore(t *testing.T) {
	t.Run("a business error keeps code, x-error-code and message; upstream headers do not follow", func(t *testing.T) {
		cause := connect.NewError(connect.CodeInvalidArgument, errors.New("unsupported currency"))
		cause.Meta().Set("x-error-code", "product.unsupported_currency")
		cause.Meta().Set("Content-Type", "application/x-from-upstream")
		cause.Meta().Set("Traceparent", "00-upstream-upstream-01")
		f := rpc.NewForwarder(&upstream{err: cause}, config.Auth{})

		_, err := f.UpsertProduct(context.Background(), upsertReq())
		var ce *connect.Error
		if !errors.As(err, &ce) || ce.Code() != connect.CodeInvalidArgument || ce.Message() != "unsupported currency" ||
			ce.Meta().Get("x-error-code") != "product.unsupported_currency" {
			t.Fatalf("err = %v, want INVALID_ARGUMENT / product.unsupported_currency / unsupported currency", err)
		}
		for _, h := range []string{"Content-Type", "Traceparent"} {
			if v := ce.Meta().Get(h); v != "" {
				t.Fatalf("upstream header %s followed the error to the caller: %q", h, v)
			}
		}
	})

	t.Run("a success is rebuilt: the upstream's response headers do not follow", func(t *testing.T) {
		f := rpc.NewForwarder(&upstream{}, config.Auth{})
		res, err := f.UpsertProduct(context.Background(), upsertReq())
		if err != nil || res.Msg.GetProduct().GetSku() != "A" {
			t.Fatalf("res = %v, err = %v", res, err)
		}
		for _, h := range []string{"Content-Type", "Traceparent"} {
			if v := res.Header().Get(h); v != "" {
				t.Fatalf("upstream header %s followed the response: %q", h, v)
			}
		}
	})

	t.Run("the catalog not answering is the client's UNAVAILABLE, unchanged", func(t *testing.T) {
		unavailable := connect.NewError(connect.CodeUnavailable, errors.New("the catalog could not be reached, retry the request"))
		unavailable.Meta().Set("x-error-code", "catalog.unavailable")
		f := rpc.NewForwarder(&upstream{err: unavailable}, config.Auth{})

		_, err := f.GetProduct(context.Background(), connect.NewRequest(&catalogv1.GetProductRequest{Sku: "A"}))
		var ce *connect.Error
		if !errors.As(err, &ce) || ce.Code() != connect.CodeUnavailable || ce.Meta().Get("x-error-code") != "catalog.unavailable" {
			t.Fatalf("err = %v, want UNAVAILABLE / catalog.unavailable", err)
		}
	})
}
