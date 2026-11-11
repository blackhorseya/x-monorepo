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

// Through the wired graph: the rate limit sits in the chain and its
// rejection carries the headers the console / curl see.
func TestRateLimit_E2E(t *testing.T) {
	srv, cleanup, err := Initialize(&config.AppConfig{
		HTTP: config.HTTP{Addr: ":0", RateLimit: config.RateLimit{RPS: 1, Burst: 1}},
		Log:  config.Log{Level: "error", Format: "text"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := adminv1connect.NewAdminServiceClient(connect.NewClient(connecthttp.NewTransport(http.DefaultClient, ts.URL), servertest.WithErrorMeta()))
	c := context.Background()

	// First call uses the only token (FailedPrecondition admin.no_database is the service's answer: it ran).
	_, err = client.ListPoison(c, &adminv1.ListPoisonRequest{})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("first call = %v, want FailedPrecondition", err)
	}
	_, err = client.ListPoison(c, &adminv1.ListPoisonRequest{})
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeResourceExhausted {
		t.Fatalf("second call = %v, want ResourceExhausted", err)
	}
	if servertest.ErrorMeta(err).Get("x-error-code") != server.RateLimitedCode || servertest.ErrorMeta(err).Get(server.RetryAfterHeader) == "" || servertest.ErrorMeta(err).Get(server.RequestIDHeader) == "" {
		t.Fatalf("rejection headers = %v", servertest.ErrorMeta(err))
	}
}
