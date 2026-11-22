package acceptance

import (
	"context"
	"errors"
	"time"
)

// Eventually polls check until it reports ok. It returns how long it waited.
func Eventually(c context.Context, what string, limit time.Duration, check func(context.Context) (observed string, ok bool, err error)) (time.Duration, error) {
	return 0, errors.New("not implemented")
}
