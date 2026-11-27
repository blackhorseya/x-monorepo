package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	adminv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/admin/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contract/admin/v1/adminv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server/servertest"
)

// Authorization is decided before the request is looked at: GetPoison with an empty request breaks two validation
// rules, and a caller the declaration does not admit is told about its permission, never about those rules.
func TestAuthz_RefusedBeforeValidation(t *testing.T) {
	const noRoleKey = "no-role-key-0123456789"
	const adminKey = "admin-role-key-0123456789"
	for _, tt := range []struct {
		name, mode, key string
		code            connect.Code
		errorCode       string
	}{
		{"required, no matching condition", config.AuthRequired, noRoleKey, connect.CodePermissionDenied, server.PermissionDeniedCode},
		{"optional, no matching condition", config.AuthOptional, noRoleKey, connect.CodePermissionDenied, server.PermissionDeniedCode},
		{"optional, no credential", config.AuthOptional, "", connect.CodeUnauthenticated, server.UnauthenticatedCode},
		// An admitted caller gets as far as validation; with auth off nothing is mounted to stop one.
		{"off, no credential", config.AuthOff, "", connect.CodeInvalidArgument, "request.validation_failed"},
		{"off, no matching role", config.AuthOff, noRoleKey, connect.CodeInvalidArgument, "request.validation_failed"},
		{"optional, admitted", config.AuthOptional, adminKey, connect.CodeInvalidArgument, "request.validation_failed"},
		{"required, no credential", config.AuthRequired, "", connect.CodeUnauthenticated, server.UnauthenticatedCode},
		{"required, admitted", config.AuthRequired, adminKey, connect.CodeInvalidArgument, "request.validation_failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv, cleanup, err := Initialize(&config.AppConfig{
				HTTP: config.HTTP{Addr: ":0"},
				Log:  config.Log{Level: "error", Format: "text"},
				Auth: config.Auth{Mode: tt.mode, APIKeys: "nobody=" + noRoleKey + ",ops=" + adminKey + ":admin"},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()
			opts := []connect.ClientInterceptor{servertest.WithErrorMeta()}
			if tt.key != "" {
				opts = append(opts, servertest.WithHeader(server.APIKeyHeader, tt.key))
			}
			client := adminv1connect.NewAdminServiceClient(connect.NewClient(connecthttp.NewTransport(http.DefaultClient, ts.URL), opts...))

			_, err = client.GetPoison(context.Background(), &adminv1.GetPoisonRequest{})
			var ce *connect.Error
			if !errors.As(err, &ce) || ce.Code() != tt.code || servertest.ErrorMeta(err).Get("x-error-code") != tt.errorCode {
				t.Fatalf("GetPoison = %v (x-error-code %q), want %s / %s", err, servertest.ErrorMeta(err).Get("x-error-code"), tt.code, tt.errorCode)
			}
		})
	}
}
