package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"

	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1/orderingv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	adminv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/admin/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contract/admin/v1/adminv1connect"
	commonv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/common/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// auth.mode required through the whole wired server: probes and docs stay
// open, an RPC without a credential is Unauthenticated with x-error-code, an
// API key and a JWT both get through, a public procedure needs nothing.
func TestAuth_Required(t *testing.T) {
	const secret = "e2e-secret-e2e-secret-e2e-secret"
	srv, cleanup, err := Initialize(&config.AppConfig{
		HTTP: config.HTTP{Addr: ":0", Docs: true},
		Log:  config.Log{Level: "error", Format: "text"},
		Auth: config.Auth{
			Mode:    config.AuthRequired,
			APIKeys: "console=console-key-0123456789,ops=ops-key-0123456789abc:admin",
			JWT:     config.JWT{HS256Secret: secret, Issuer: "x-monorepo"},
			Public:  []string{orderingv1connect.OrderServiceGetOrderProcedure},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := context.Background()

	for _, p := range []string{server.HealthzPath, server.ReadyzPath, server.MetricsPath, server.DocsPath} {
		res, err := http.Get(ts.URL + p)
		if err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %v, %v (must stay open)", p, res, err)
		}
		_ = res.Body.Close()
	}

	withHeader := func(k, v string) connect.ClientOption {
		return connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(c context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				req.Header().Set(k, v)
				return next(c, req)
			}
		}))
	}
	create := &orderingv1.CreateOrderRequest{CustomerId: "c-1", Items: []*orderingv1.LineItem{
		{Sku: "SKU-1", Quantity: 1, UnitPrice: &commonv1.Money{AmountMinor: 100, Currency: "USD"}},
	}}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "u-1", "iss": "x-monorepo", "exp": time.Now().Add(time.Minute).Unix(),
	}).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}

	anon := orderingv1connect.NewOrderServiceClient(http.DefaultClient, ts.URL)
	_, err = anon.CreateOrder(c, connect.NewRequest(create))
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeUnauthenticated || ce.Meta().Get("x-error-code") != server.UnauthenticatedCode {
		t.Fatalf("anonymous CreateOrder = %v, want unauthenticated %s", err, server.UnauthenticatedCode)
	}
	if ce.Meta().Get(server.RequestIDHeader) == "" {
		t.Fatal("rejected call must still carry X-Request-Id")
	}

	for name, client := range map[string]orderingv1connect.OrderServiceClient{
		"api key": orderingv1connect.NewOrderServiceClient(http.DefaultClient, ts.URL, withHeader(server.APIKeyHeader, "console-key-0123456789")),
		"jwt":     orderingv1connect.NewOrderServiceClient(http.DefaultClient, ts.URL, withHeader(server.AuthorizationHeader, "Bearer "+token)),
	} {
		created, err := client.CreateOrder(c, connect.NewRequest(create))
		if err != nil {
			t.Fatalf("%s CreateOrder: %v", name, err)
		}
		// GetOrder is public: the anonymous client reads what the authenticated one wrote.
		got, err := anon.GetOrder(c, connect.NewRequest(&orderingv1.GetOrderRequest{Id: created.Msg.GetOrder().GetId()}))
		if err != nil || got.Msg.GetOrder().GetId() != created.Msg.GetOrder().GetId() {
			t.Fatalf("%s: public GetOrder = %v, %v", name, got, err)
		}
	}

	bad := orderingv1connect.NewOrderServiceClient(http.DefaultClient, ts.URL, withHeader(server.AuthorizationHeader, "Bearer "+token+"x"))
	if _, err := bad.CreateOrder(c, connect.NewRequest(create)); !errors.As(err, &ce) || ce.Meta().Get("x-error-code") != server.InvalidCredentialCode {
		t.Fatalf("tampered token = %v", err)
	}

	// Authorization on the admin surface: the role comes from the JWT's
	// roles claim or the API key's ":role" suffix. No database is wired here,
	// so a caller that passes the role check gets FailedPrecondition
	// admin.no_database — proof the guard let it through.
	adminToken, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "u-2", "iss": "x-monorepo", "exp": time.Now().Add(time.Minute).Unix(), "roles": []string{"ops", "admin"},
	}).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct {
		header, value string
		code          connect.Code
		errorCode     string
	}{
		"anonymous":        {"", "", connect.CodeUnauthenticated, server.UnauthenticatedCode},
		"jwt without role": {server.AuthorizationHeader, "Bearer " + token, connect.CodePermissionDenied, server.PermissionDeniedCode},
		"api key no role":  {server.APIKeyHeader, "console-key-0123456789", connect.CodePermissionDenied, server.PermissionDeniedCode},
		"jwt roles claim":  {server.AuthorizationHeader, "Bearer " + adminToken, connect.CodeFailedPrecondition, "admin.no_database"},
		"api key :admin":   {server.APIKeyHeader, "ops-key-0123456789abc", connect.CodeFailedPrecondition, "admin.no_database"},
	} {
		var opts []connect.ClientOption
		if tt.header != "" {
			opts = append(opts, withHeader(tt.header, tt.value))
		}
		admin := adminv1connect.NewAdminServiceClient(http.DefaultClient, ts.URL, opts...)
		_, err := admin.ListPoison(c, connect.NewRequest(&adminv1.ListPoisonRequest{}))
		if !errors.As(err, &ce) || ce.Code() != tt.code || ce.Meta().Get("x-error-code") != tt.errorCode {
			t.Fatalf("%s: ListPoison = %v, want %s / %s", name, err, tt.code, tt.errorCode)
		}
	}
}
