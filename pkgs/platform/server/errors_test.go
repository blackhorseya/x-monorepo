package server_test

import (
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

func TestToConnectError(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantCode connect.Code
		wantMeta string
	}{
		{"kernel kind", fmt.Errorf("wrap: %w", kernel.NewError(kernel.KindNotFound, "x.not_found", "nope")), connect.CodeNotFound, "x.not_found"},
		{"conflict", kernel.NewError(kernel.KindConflict, "x.dup", "dup"), connect.CodeAlreadyExists, "x.dup"},
		{"permission denied", kernel.NewError(kernel.KindPermissionDenied, "x.denied", "no"), connect.CodePermissionDenied, "x.denied"},
		{"optimistic lock loss", fmt.Errorf("save x: %w", fmt.Errorf("x: stale: %w", kernel.ErrConcurrentUpdate)), connect.CodeAborted, server.ConcurrentUpdateCode},
		{"unknown hides message", errors.New("dial tcp: secret host"), connect.CodeInternal, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ce *connect.Error
			if !errors.As(server.ToConnectError(tc.err), &ce) {
				t.Fatal("not a *connect.Error")
			}
			if ce.Code() != tc.wantCode {
				t.Fatalf("code = %v, want %v", ce.Code(), tc.wantCode)
			}
			if got := ce.Meta().Get("x-error-code"); got != tc.wantMeta {
				t.Fatalf("x-error-code = %q, want %q", got, tc.wantMeta)
			}
			if tc.wantCode == connect.CodeInternal && ce.Message() != "internal error" {
				t.Fatalf("internal message leaked: %q", ce.Message())
			}
		})
	}
}
