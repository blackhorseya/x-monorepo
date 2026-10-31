package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1/orderingv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// Through the wired graph: the rate limit sits in the chain and its
// rejection carries the headers the console / curl see.
func TestRateLimit_E2E(t *testing.T) {
	srv, cleanup, err := initialize(t, &config.AppConfig{
		HTTP: config.HTTP{Addr: ":0", RateLimit: config.RateLimit{RPS: 1, Burst: 1}},
		Log:  config.Log{Level: "error", Format: "text"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := orderingv1connect.NewOrderServiceClient(http.DefaultClient, ts.URL)
	c := context.Background()

	// First call uses the only token (NotFound is the use case's answer: it ran).
	_, err = client.GetOrder(c, connect.NewRequest(&orderingv1.GetOrderRequest{Id: "nope"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("first call = %v, want NotFound", err)
	}
	_, err = client.GetOrder(c, connect.NewRequest(&orderingv1.GetOrderRequest{Id: "nope"}))
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeResourceExhausted {
		t.Fatalf("second call = %v, want ResourceExhausted", err)
	}
	if ce.Meta().Get("x-error-code") != server.RateLimitedCode || ce.Meta().Get(server.RetryAfterHeader) == "" || ce.Meta().Get(server.RequestIDHeader) == "" {
		t.Fatalf("rejection headers = %v", ce.Meta())
	}
}
