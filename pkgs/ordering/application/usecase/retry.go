package usecase

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

const (
	// maxAttempts bounds the load → apply → save loop: the first try plus two retries.
	maxAttempts = 3
	// maxJitter spreads concurrent losers apart so they don't collide again in lockstep.
	maxJitter = 20 * time.Millisecond
)

// retryOnConflict runs attempt until it stops failing with an optimistic-lock
// conflict (kernel.ErrConcurrentUpdate). attempt must reload the aggregate every
// time, so the domain rule is re-applied to what the winner saved (a cancel that
// lost to a cancel now gets ErrCannotCancel, not a conflict). Any other error, or
// a spent budget, returns as-is; a cancelled context stops the wait early.
func retryOnConflict(c context.Context, attempt func() error) error {
	var err error
	for n := 1; n <= maxAttempts; n++ {
		if err = attempt(); !errors.Is(err, kernel.ErrConcurrentUpdate) {
			return err
		}
		if n == maxAttempts {
			break
		}

		t := time.NewTimer(rand.N(maxJitter))
		select {
		case <-c.Done():
			t.Stop()

			return err
		case <-t.C:
		}
	}

	return err
}
