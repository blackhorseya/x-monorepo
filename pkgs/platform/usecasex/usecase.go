// Package usecasex defines the generic use-case shape (go-kit's "endpoint")
// and decorators for cross-cutting concerns. Adapters depend on UseCase[I, O],
// never on a concrete usecase struct, so decorators can wrap transparently and
// every driving adapter (rpc, consumer, cron) gets the same logging/metrics.
package usecasex

import "context"

// UseCase is a single application operation: one input, one output.
type UseCase[I, O any] interface {
	Execute(c context.Context, in I) (O, error)
}

// Func adapts a plain function to UseCase.
type Func[I, O any] func(c context.Context, in I) (O, error)

func (f Func[I, O]) Execute(c context.Context, in I) (O, error) { return f(c, in) }

// Decorator wraps a use case with extra behaviour.
type Decorator[I, O any] func(next UseCase[I, O]) UseCase[I, O]

// Chain applies decorators so the first listed is the outermost.
func Chain[I, O any](uc UseCase[I, O], decorators ...Decorator[I, O]) UseCase[I, O] {
	for i := len(decorators) - 1; i >= 0; i-- {
		uc = decorators[i](uc)
	}

	return uc
}
