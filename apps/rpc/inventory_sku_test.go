package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	inventoryv1 "github.com/blackhorseya/x-monorepo/pkgs/inventory/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/contract/v1/inventoryv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

// SetStock takes catalog's SKU rule (1–64 bytes): the proto rejects more than 64
// characters before the handler runs; a SKU of at most 64 characters but more
// than 64 bytes gets past the proto and is refused by the domain.
func TestInventory_SetStockSKUFormat(t *testing.T) {
	srv, cleanup, err := Initialize(&config.AppConfig{
		HTTP: config.HTTP{Addr: ":0"},
		Log:  config.Log{Level: "error", Format: "text"},
		Auth: config.Auth{Mode: config.AuthOff},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	client := inventoryv1connect.NewInventoryServiceClient(http.DefaultClient, ts.URL)

	set := func(sku string) error {
		_, err := client.SetStock(context.Background(), connect.NewRequest(&inventoryv1.SetStockRequest{Sku: sku, OnHand: 1}))
		return err
	}
	wantCode := func(t *testing.T, err error, code connect.Code, errorCode string) {
		t.Helper()
		var ce *connect.Error
		if !errors.As(err, &ce) || ce.Code() != code || ce.Meta().Get("x-error-code") != errorCode {
			t.Fatalf("err = %v, want %s / %s", err, code, errorCode)
		}
	}

	if err := set(strings.Repeat("a", 64)); err != nil {
		t.Fatalf("64 bytes: %v", err)
	}
	wantCode(t, set(strings.Repeat("a", 65)), connect.CodeInvalidArgument, "request.validation_failed")
	wantCode(t, set(strings.Repeat("中", 22)), connect.CodeInvalidArgument, "stock.invalid_sku") // 22 characters, 66 bytes
}
