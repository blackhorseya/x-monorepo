package otel

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

func TestFanout_AllHandlersSeeRecordWithAttrsAndGroup(t *testing.T) {
	var a, b bytes.Buffer
	h := &fanout{level: slog.LevelInfo, handlers: []slog.Handler{
		slog.NewTextHandler(&a, &slog.HandlerOptions{Level: slog.LevelDebug}),
		slog.NewJSONHandler(&b, &slog.HandlerOptions{Level: slog.LevelDebug}),
	}}
	l := slog.New(h).With("req", "r1").WithGroup("g")

	l.Debug("dropped by the shared gate")
	l.Info("hello", "k", "v")

	for name, buf := range map[string]*bytes.Buffer{"text": &a, "json": &b} {
		out := buf.String()
		if strings.Count(out, "\n") != 1 {
			t.Fatalf("%s: want exactly one record, got:\n%s", name, out)
		}
		for _, want := range []string{"hello", "req", "r1", "g.k", "v"} {
			if !strings.Contains(strings.ReplaceAll(out, `"g":{"k"`, "g.k"), want) {
				t.Fatalf("%s: missing %q in %s", name, want, out)
			}
		}
	}
}

func TestNewLogger_LevelAndFormatFromConfig(t *testing.T) {
	l := newLogger(config.Log{Level: "warn", Format: "text"}, nil)
	if l.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("info should be disabled at level warn")
	}
	if !l.Enabled(context.Background(), slog.LevelWarn) {
		t.Fatal("warn should be enabled")
	}
}
