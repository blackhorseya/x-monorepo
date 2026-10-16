package usecasex

import (
	"context"
	"errors"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// Logging logs one line per execution with duration and the domain error kind.
// name should be stable, e.g. "ordering.cancel_order".
func Logging[I, O any](name string) Decorator[I, O] {
	return func(next UseCase[I, O]) UseCase[I, O] {
		return Func[I, O](func(c context.Context, in I) (O, error) {
			ctx := contextx.FromContext(c)
			start := time.Now()

			out, err := next.Execute(ctx, in)

			attrs := []any{"usecase", name, "duration_ms", time.Since(start).Milliseconds()}
			if err == nil {
				ctx.Info("usecase ok", attrs...)
				return out, nil
			}

			var de *kernel.Error
			if errors.As(err, &de) {
				// Domain errors are expected outcomes, not incidents.
				ctx.Info("usecase rejected", append(attrs, "kind", de.Kind().String(), "code", de.Code())...)
			} else {
				ctx.Error("usecase failed", append(attrs, "error", err)...)
			}

			return out, err
		})
	}
}
