package contextx_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

type ctxKey struct{}

// capture records the context each Handle call received.
type capture struct{ seen []context.Context }

func (h *capture) Enabled(context.Context, slog.Level) bool { return true }
func (h *capture) Handle(c context.Context, _ slog.Record) error {
	h.seen = append(h.seen, c)
	return nil
}
func (h *capture) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capture) WithGroup(string) slog.Handler      { return h }

func TestContextx_LogMethodsPassTheContext(t *testing.T) {
	h := &capture{}
	c := context.WithValue(context.Background(), ctxKey{}, "marker")
	ctx := contextx.FromContext(contextx.WithLogger(c, slog.New(h)))

	ctx.Debug("d")
	ctx.Info("i")
	ctx.Warn("w")
	ctx.Error("e")

	if len(h.seen) != 4 {
		t.Fatalf("records = %d, want 4", len(h.seen))
	}
	for i, got := range h.seen {
		if got.Value(ctxKey{}) != "marker" {
			t.Fatalf("record %d: handler did not receive the request context", i)
		}
	}
}
