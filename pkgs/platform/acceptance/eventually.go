package acceptance

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// pollEvery is how long Eventually waits between two looks at the condition.
const pollEvery = 100 * time.Millisecond

// Eventually polls check until it reports ok, and returns how long it waited.
//
// The wait is bounded by limit and by the deadline of c, whichever is nearer: a step must not outlive the test that
// runs it, so the runner gives its steps a context that ends before go test's own -timeout. check gets a context with the
// same bound. It returns what it saw (observed), whether the condition holds (ok), and an error only when it cannot
// look at all (a failed query): that ends the wait at once.
//
// On timeout the error says what was awaited, the bound, and the last observed value. observed reaches the test output,
// so it holds counts and names, never a URL or a credential.
func Eventually(c context.Context, what string, limit time.Duration, check func(context.Context) (observed string, ok bool, err error)) (time.Duration, error) {
	start := time.Now()
	bound, byDeadline := limit, false
	if d, ok := c.Deadline(); ok {
		if rest := time.Until(d); rest < bound {
			bound, byDeadline = rest, true
		}
	}
	ec, cancel := context.WithTimeout(c, max(bound, 0))
	defer cancel()

	last := "nothing yet"
	for {
		observed, ok, err := check(ec)
		if err != nil && ec.Err() == nil {
			return time.Since(start), fmt.Errorf("eventually: %s: %w", what, err)
		}
		if err == nil {
			if observed != "" {
				last = observed
			}
			if ok {
				return time.Since(start), nil
			}
		}
		timer := time.NewTimer(pollEvery)
		select {
		case <-ec.Done():
			timer.Stop()
		case <-timer.C:
			continue
		}

		break
	}

	waited := time.Since(start)
	if cerr := c.Err(); cerr != nil && !errors.Is(cerr, context.DeadlineExceeded) {
		return waited, fmt.Errorf("eventually: %s: stopped after %s: %w; last observed: %s", what, waited.Round(time.Millisecond), cerr, last)
	}
	why := fmt.Sprintf("limit %s", limit)
	if byDeadline {
		why = fmt.Sprintf("limit %s, bounded by the test deadline", limit)
	}

	return waited, fmt.Errorf("eventually: %s did not hold after %s (%s); last observed: %s", what, waited.Round(time.Millisecond), why, last)
}
