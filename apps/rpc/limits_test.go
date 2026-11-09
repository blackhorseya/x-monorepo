package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	identityv1 "github.com/blackhorseya/x-monorepo/pkgs/identity/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/contract/v1/identityv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
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
	client := identityv1connect.NewIdentityServiceClient(http.DefaultClient, ts.URL)
	c := context.Background()

	// First call uses the only token (NotFound is the use case's answer: it ran).
	_, err = client.GetAccount(c, connect.NewRequest(&identityv1.GetAccountRequest{Id: "nope"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("first call = %v, want NotFound", err)
	}
	_, err = client.GetAccount(c, connect.NewRequest(&identityv1.GetAccountRequest{Id: "nope"}))
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeResourceExhausted {
		t.Fatalf("second call = %v, want ResourceExhausted", err)
	}
	if ce.Meta().Get("x-error-code") != server.RateLimitedCode || ce.Meta().Get(server.RetryAfterHeader) == "" || ce.Meta().Get(server.RequestIDHeader) == "" {
		t.Fatalf("rejection headers = %v", ce.Meta())
	}
}
