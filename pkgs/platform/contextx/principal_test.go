package contextx_test

import (
	"context"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

func TestPrincipal(t *testing.T) {
	c := context.Background()
	if p := contextx.PrincipalFrom(c); !p.IsAnonymous() || p.HasRole("admin") {
		t.Fatalf("no principal = %+v", p)
	}

	p := contextx.Principal{Subject: "u-1", Name: "Ann", Roles: []string{"admin"}, Method: "jwt"}
	c = contextx.WithPrincipal(c, p)
	got := contextx.PrincipalFrom(contextx.FromContext(c)) // survives the Contextx wrapper
	if got.IsAnonymous() || got.Subject != "u-1" || !got.HasRole("admin") || got.HasRole("ops") || got.Method != "jwt" {
		t.Fatalf("principal = %+v", got)
	}
}
