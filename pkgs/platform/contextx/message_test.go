package contextx_test

import (
	"context"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

func TestMessageID(t *testing.T) {
	if got := contextx.MessageIDFrom(context.Background()); got != "" {
		t.Fatalf("outside a consumer: %q, want empty", got)
	}
	c := contextx.WithMessageID(context.Background(), "m-1")
	if got := contextx.MessageIDFrom(c); got != "m-1" {
		t.Fatalf("MessageIDFrom = %q", got)
	}
}
