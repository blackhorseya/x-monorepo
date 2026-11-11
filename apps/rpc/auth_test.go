package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/golang-jwt/jwt/v5"

	identityv1 "github.com/blackhorseya/x-monorepo/pkgs/identity/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/contract/v1/identityv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	adminv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/admin/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contract/admin/v1/adminv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server/servertest"
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
			Public:  []string{identityv1connect.IdentityServiceWhoAmIProcedure},
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

	withHeader := servertest.WithHeader
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "u-1", "iss": "x-monorepo", "exp": time.Now().Add(time.Minute).Unix(),
	}).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}

	// ListAccounts is behind the admin role and is not public: it is the procedure the credentials are tried on.
	list := &identityv1.ListAccountsRequest{}
	anon := identityv1connect.NewIdentityServiceClient(connect.NewClient(connecthttp.NewTransport(http.DefaultClient, ts.URL, connecthttp.WithReadMaxBytes(0)), servertest.WithErrorMeta()))
	_, err = anon.ListAccounts(c, list)
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeUnauthenticated || servertest.ErrorMeta(err).Get("x-error-code") != server.UnauthenticatedCode {
		t.Fatalf("anonymous ListAccounts = %v, want unauthenticated %s", err, server.UnauthenticatedCode)
	}
	if servertest.ErrorMeta(err).Get(server.RequestIDHeader) == "" {
		t.Fatal("rejected call must still carry X-Request-Id")
	}

	// A public procedure needs nothing: WhoAmI answers anonymous instead of refusing.
	if who, err := anon.WhoAmI(c, &identityv1.WhoAmIRequest{}); err != nil || !who.GetAnonymous() {
		t.Fatalf("public WhoAmI = %v, %v, want anonymous", who, err)
	}

	// An API key and a JWT both get through authentication; the admin one also passes the role check.
	for name, tt := range map[string]struct {
		opt  connect.ClientInterceptor
		code connect.Code
	}{
		"api key without role": {withHeader(server.APIKeyHeader, "console-key-0123456789"), connect.CodePermissionDenied},
		"jwt without role":     {withHeader(server.AuthorizationHeader, "Bearer "+token), connect.CodePermissionDenied},
		"api key :admin":       {withHeader(server.APIKeyHeader, "ops-key-0123456789abc"), 0},
	} {
		client := identityv1connect.NewIdentityServiceClient(connect.NewClient(connecthttp.NewTransport(http.DefaultClient, ts.URL, connecthttp.WithReadMaxBytes(0)), servertest.WithErrorMeta(), tt.opt))
		_, err := client.ListAccounts(c, list)
		if tt.code == 0 {
			if err != nil {
				t.Fatalf("%s ListAccounts: %v", name, err)
			}
			continue
		}
		if connect.CodeOf(err) != tt.code {
			t.Fatalf("%s ListAccounts = %v, want %s", name, err, tt.code)
		}
	}

	bad := identityv1connect.NewIdentityServiceClient(connect.NewClient(connecthttp.NewTransport(http.DefaultClient, ts.URL, connecthttp.WithReadMaxBytes(0)), servertest.WithErrorMeta(), withHeader(server.AuthorizationHeader, "Bearer "+token+"x")))
	if _, err := bad.ListAccounts(c, list); !errors.As(err, &ce) || servertest.ErrorMeta(err).Get("x-error-code") != server.InvalidCredentialCode {
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
		opts := []connect.ClientInterceptor{servertest.WithErrorMeta()}
		if tt.header != "" {
			opts = append(opts, withHeader(tt.header, tt.value))
		}
		admin := adminv1connect.NewAdminServiceClient(connect.NewClient(connecthttp.NewTransport(http.DefaultClient, ts.URL, connecthttp.WithReadMaxBytes(0)), opts...))
		_, err := admin.ListPoison(c, &adminv1.ListPoisonRequest{})
		if !errors.As(err, &ce) || ce.Code() != tt.code || servertest.ErrorMeta(err).Get("x-error-code") != tt.errorCode {
			t.Fatalf("%s: ListPoison = %v, want %s / %s", name, err, tt.code, tt.errorCode)
		}
	}
}
