package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	identityv1 "github.com/blackhorseya/x-monorepo/pkgs/identity/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/contract/v1/identityv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// New ids are UUID v7, seen from the real entry: the account id CreateAccount
// returns and the X-Request-Id the server mints. A request id the client
// brings is echoed untouched, whatever it looks like. Ids stay opaque strings.
func TestIDs_NewOnesAreV7FromTheRealEntry(t *testing.T) {
	srv, cleanup, err := Initialize(&config.AppConfig{
		HTTP: config.HTTP{Addr: ":0"},
		Log:  config.Log{Level: "error", Format: "text"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := identityv1connect.NewIdentityServiceClient(http.DefaultClient, ts.URL)

	create := func(rid string) *connect.Response[identityv1.CreateAccountResponse] {
		t.Helper()
		req := connect.NewRequest(&identityv1.CreateAccountRequest{Name: "v7"})
		if rid != "" {
			req.Header().Set(server.RequestIDHeader, rid)
		}
		res, err := client.CreateAccount(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}

		return res
	}
	isV7 := func(id string) bool {
		u, err := uuid.Parse(id)

		return err == nil && u.Version() == 7
	}

	res := create("")
	if id := res.Msg.GetAccount().GetId(); !isV7(id) {
		t.Fatalf("account id %q is not a UUID v7", id)
	}
	if rid := res.Header().Get(server.RequestIDHeader); !isV7(rid) {
		t.Fatalf("minted X-Request-Id %q is not a UUID v7", rid)
	}

	// A client's own value, even a v4 one, comes back exactly as sent.
	const v4 = "6ba7b810-9dad-41d1-80b4-00c04fd430c8"
	for _, own := range []string{v4, "client-42"} {
		if got := create(own).Header().Get(server.RequestIDHeader); got != own {
			t.Fatalf("client X-Request-Id %q came back as %q", own, got)
		}
	}
}
