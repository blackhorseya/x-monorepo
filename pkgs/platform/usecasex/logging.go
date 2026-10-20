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
//
// Success is Debug: on the rpc path the Connect interceptor already logs one
// "rpc" line per request, and usecase.duration{kind="ok"} counts successes on
// every path. Outcomes worth reading stay visible: a domain rejection is Info,
// a lost optimistic-lock race Warn, anything else Error.
func Logging[I, O any](name string) Decorator[I, O] {
	return func(next UseCase[I, O]) UseCase[I, O] {
		return Func[I, O](func(c context.Context, in I) (O, error) {
			ctx := contextx.FromContext(c)
			start := time.Now()

			out, err := next.Execute(ctx, in)

			attrs := []any{"usecase", name, "duration_ms", time.Since(start).Milliseconds()}
			if p := contextx.PrincipalFrom(ctx); !p.IsAnonymous() {
				attrs = append(attrs, "sub", p.Subject)
			}
			if err == nil {
				ctx.Debug("usecase ok", attrs...)
				return out, nil
			}

			var de *kernel.Error
			switch {
			case errors.Is(err, kernel.ErrConcurrentUpdate):
				// A lost optimistic-lock race: expected under contention, retried by the caller.
				ctx.Warn("usecase aborted", append(attrs, "error", err)...)
			case errors.As(err, &de):
				// Domain errors are expected outcomes, not incidents.
				ctx.Info("usecase rejected", append(attrs, "kind", de.Kind().String(), "code", de.Code())...)
			default:
				ctx.Error("usecase failed", append(attrs, "error", err)...)
			}

			return out, err
		})
	}
}
