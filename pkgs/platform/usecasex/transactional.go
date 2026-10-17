package usecasex

import "context"

// Transactor runs fn in one unit of work. Each bounded context's
// application/port.Transactor satisfies it structurally; they stay separate
// named types so wire can tell the contexts apart.
type Transactor interface {
	Do(c context.Context, fn func(c context.Context) error) error
}

// Transactional runs the use case inside tx.Do: everything the use case
// persists or publishes through the context it receives commits or rolls back
// together, so use cases never open transactions themselves.
//
// Any error from Do, including a failed commit, discards the use case's
// output: a caller must never see a result whose writes did not land.
func Transactional[I, O any](tx Transactor) Decorator[I, O] {
	return func(next UseCase[I, O]) UseCase[I, O] {
		return Func[I, O](func(c context.Context, in I) (O, error) {
			var out O
			err := tx.Do(c, func(c context.Context) error {
				var err error
				out, err = next.Execute(c, in)

				return err
			})
			if err != nil {
				var zero O
				return zero, err
			}

			return out, nil
		})
	}
}
