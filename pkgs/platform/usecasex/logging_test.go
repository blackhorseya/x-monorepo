package usecasex_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// levels records the level and message of every record it receives.
type levels struct{ got []string }

func (*levels) Enabled(context.Context, slog.Level) bool { return true }
func (h *levels) Handle(_ context.Context, r slog.Record) error {
	h.got = append(h.got, r.Level.String()+" "+r.Message)
	return nil
}
func (h *levels) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *levels) WithGroup(string) slog.Handler      { return h }

func TestLogging_Levels(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"success is debug", nil, "DEBUG usecase ok"},
		{"domain rejection is info", kernel.NewError(kernel.KindNotFound, "x.not_found", "nope"), "INFO usecase rejected"},
		{"lost race is warn", fmt.Errorf("save: %w", kernel.ErrConcurrentUpdate), "WARN usecase aborted"},
		{"infra failure is error", errors.New("boom"), "ERROR usecase failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &levels{}
			c := contextx.WithLogger(context.Background(), slog.New(h))
			uc := usecasex.Chain[int, int](
				usecasex.Func[int, int](func(context.Context, int) (int, error) { return 0, tc.err }),
				usecasex.Logging[int, int]("test"),
			)
			_, _ = uc.Execute(c, 1)
			if len(h.got) != 1 || h.got[0] != tc.want {
				t.Fatalf("logged %v, want [%s]", h.got, tc.want)
			}
		})
	}
}
